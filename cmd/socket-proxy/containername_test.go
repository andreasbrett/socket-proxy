package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"regexp"
	"strings"
	"testing"

	"github.com/wollomatic/socket-proxy/internal/config"
)

type inspectionTransport func(*http.Request) (*http.Response, error)

func (transport inspectionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestContainerNameAllowed(t *testing.T) {
	for _, test := range []struct {
		name string
		want bool
	}{
		{"db_1", true}, {"/db_2", true}, {"redis", true}, {"redis-cache", true},
		{"web-db", false}, {"DB_1", false}, {"", false},
		{"/db_1/web", false},
	} {
		if got := containerNameAllowed([]string{"db", "redis"}, test.name); got != test.want {
			t.Errorf("containerNameAllowed(%q) = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestContainerLabelsAllowed(t *testing.T) {
	for _, test := range []struct {
		name         string
		requirements []string
		labels       map[string]string
		want         bool
	}{
		{name: "disabled", want: true},
		{name: "missing", requirements: []string{"myfoo"}},
		{name: "present with empty value", requirements: []string{"myfoo"}, labels: map[string]string{"myfoo": ""}, want: true},
		{name: "both match", requirements: []string{"myfoo", "com.bar=fun"}, labels: map[string]string{"myfoo": "yes", "com.bar": "fun"}, want: true},
		{name: "all required", requirements: []string{"myfoo", "com.bar=fun"}, labels: map[string]string{"com.bar": "fun"}},
		{name: "wrong value", requirements: []string{"com.bar=fun"}, labels: map[string]string{"com.bar": "other"}},
		{name: "empty value matches", requirements: []string{"myfoo="}, labels: map[string]string{"myfoo": ""}, want: true},
		{name: "empty value requires presence", requirements: []string{"myfoo="}},
		{name: "exact value", requirements: []string{"com.bar=fun"}, labels: map[string]string{"com.bar": "fun-extra"}},
		{name: "case sensitive", requirements: []string{"com.bar=fun"}, labels: map[string]string{"com.bar": "FUN"}},
		{name: "equals in value", requirements: []string{"com.bar=a=b"}, labels: map[string]string{"com.bar": "a=b"}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			filter := containerFilter{labels: test.requirements}
			if got := filter.labelsAllowed(test.labels); got != test.want {
				t.Errorf("labelsAllowed() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCheckContainerLabelRestrictions(t *testing.T) {
	previousProxy := socketProxy
	t.Cleanup(func() { socketProxy = previousProxy })
	for _, test := range []struct {
		name       string
		method     string
		path       string
		body       string
		inspection string
		prefixes   []string
		wantErr    bool
	}{
		{name: "label only permits any name", method: "GET", path: "/containers/abc/json", inspection: `{"Id":"abc","Name":"/web","Config":{"Labels":{"myfoo":"","com.bar":"fun"}}}`},
		{name: "missing label", method: "GET", path: "/containers/abc/json", inspection: `{"Id":"abc","Config":{"Labels":{"com.bar":"fun"}}}`, wantErr: true},
		{name: "wrong label value", method: "GET", path: "/containers/abc/json", inspection: `{"Id":"abc","Config":{"Labels":{"myfoo":"yes","com.bar":"wrong"}}}`, wantErr: true},
		{name: "missing config", method: "GET", path: "/containers/abc/json", inspection: `{"Id":"abc"}`, wantErr: true},
		{name: "name and labels match", method: "POST", path: "/v1.54/containers/db_1/stop", prefixes: []string{"db"}, inspection: `{"Id":"abc","Name":"/db_1","Config":{"Labels":{"myfoo":"yes","com.bar":"fun"}}}`},
		{name: "name also required", method: "GET", path: "/containers/abc/json", prefixes: []string{"db"}, inspection: `{"Id":"abc","Name":"/web","Config":{"Labels":{"myfoo":"yes","com.bar":"fun"}}}`, wantErr: true},
		{name: "create with explicit labels", method: "POST", path: "/containers/create", body: `{"Image":"test","Labels":{"myfoo":"","com.bar":"fun"}}`},
		{name: "create missing labels", method: "POST", path: "/containers/create", body: `{"Image":"test"}`, wantErr: true},
		{name: "create wrong labels", method: "POST", path: "/containers/create", body: `{"Labels":{"myfoo":"yes","com.bar":"wrong"}}`, wantErr: true},
		{name: "create invalid JSON", method: "POST", path: "/containers/create", body: `{`, wantErr: true},
		{name: "create both filters", method: "POST", path: "/containers/create?name=db_2", prefixes: []string{"db"}, body: `{"Labels":{"myfoo":"","com.bar":"fun"}}`},
		{name: "create name required with both filters", method: "POST", path: "/containers/create", prefixes: []string{"db"}, body: `{"Labels":{"myfoo":"","com.bar":"fun"}}`, wantErr: true},
		{name: "rename labels checked", method: "POST", path: "/containers/abc/rename?name=web", inspection: `{"Id":"abc","Config":{"Labels":{"myfoo":"","com.bar":"fun"}}}`},
		{name: "prune blocked", method: "POST", path: "/containers/prune", wantErr: true},
		{name: "exec labels checked", method: "POST", path: "/exec/session/start", inspection: `{"ContainerID":"abc","Id":"abc","Config":{"Labels":{"myfoo":"","com.bar":"fun"}}}`},
		{name: "exec labels missing", method: "POST", path: "/exec/session/start", inspection: `{"ContainerID":"abc","Id":"abc"}`, wantErr: true},
		{name: "commit labels missing", method: "POST", path: "/commit?container=abc", inspection: `{"Id":"abc"}`, wantErr: true},
		{name: "network labels missing", method: "POST", path: "/networks/net/connect", body: `{"Container":"abc"}`, inspection: `{"Id":"abc"}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			socketProxy = &httputil.ReverseProxy{Transport: inspectionTransport(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.inspection))}, nil
			})}
			filter := containerFilter{prefixes: test.prefixes, labels: []string{"myfoo", "com.bar=fun"}}
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			err := filter.checkRequest(request)
			if (err != nil) != test.wantErr {
				t.Fatalf("checkRequest() error = %v, wantErr %v", err, test.wantErr)
			}
			if err == nil && strings.Contains(test.path, "/containers/create") {
				body, readErr := io.ReadAll(request.Body)
				if readErr != nil || string(body) != test.body {
					t.Errorf("creation body not preserved: %s, error = %v", body, readErr)
				}
			}
		})
	}
}

func TestFilterContainerLabelResponse(t *testing.T) {
	previousProxy := socketProxy
	t.Cleanup(func() { socketProxy = previousProxy })
	for _, test := range []struct {
		name         string
		path         string
		body         string
		prefixes     []string
		requirements []string
		want         string
	}{
		{name: "label only list", path: "/containers/json", body: `[{"Names":["/web"],"Labels":{"myfoo":"","com.bar":"fun"},"Extra":42},{"Names":["/db_1"],"Labels":{"com.bar":"fun"}},{"Names":["/db_2"],"Labels":{"myfoo":"","com.bar":"wrong"}}]`, want: `[{"Names":["/web"],"Labels":{"myfoo":"","com.bar":"fun"},"Extra":42}]`},
		{name: "both filters", path: "/v1.54/containers/json?all=true", prefixes: []string{"db"}, body: `[{"Names":["/web"],"Labels":{"myfoo":"","com.bar":"fun"}},{"Names":["/db_1"],"Labels":{"myfoo":"","com.bar":"fun"}},{"Names":["/db_2"]}]`, want: `[{"Names":["/db_1"],"Labels":{"myfoo":"","com.bar":"fun"}}]`},
		{name: "no matching labels", path: "/containers/json", body: `[{"Labels":null}]`, want: `[]`},
		{name: "events inspect actual labels", path: "/v1.54/events", body: `{"Type":"container","Actor":{"ID":"blocked","Attributes":{"myfoo":"","com.bar":"fun"}}}
{"Type":"container","Actor":{"ID":"allowed","Attributes":{"name":"db_1"}}}
{"Type":"container","Actor":{"ID":"gone","Attributes":{"myfoo":"","com.bar":"fun"}}}`, want: "{\"Type\":\"container\",\"Actor\":{\"ID\":\"allowed\",\"Attributes\":{\"name\":\"db_1\"}}}\n"},
		{name: "reserved event attribute is not a label", path: "/events", requirements: []string{"name"}, body: `{"Type":"container","Actor":{"ID":"allowed","Attributes":{"name":"db_1"}}}`, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			socketProxy = &httputil.ReverseProxy{Transport: inspectionTransport(func(request *http.Request) (*http.Response, error) {
				body := `{"Id":"blocked","Config":{"Labels":{}}}`
				status := http.StatusOK
				switch {
				case strings.Contains(request.URL.Path, "/allowed/json"):
					body = `{"Id":"allowed","Name":"/db_1","Config":{"Labels":{"myfoo":"","com.bar":"fun"}}}`
				case strings.Contains(request.URL.Path, "/gone/json"):
					status = http.StatusNotFound
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			requirements := test.requirements
			if requirements == nil {
				requirements = []string{"myfoo", "com.bar=fun"}
			}
			filter := containerFilter{prefixes: test.prefixes, labels: requirements}
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body)), Request: httptest.NewRequest(http.MethodGet, test.path, nil)}
			if err := filter.filterResponse(response); err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || string(body) != test.want {
				t.Errorf("body = %s, error = %v, want %s", body, err, test.want)
			}
		})
	}
}

func TestCheckContainerNameRestrictions(t *testing.T) {
	previousProxy := socketProxy
	t.Cleanup(func() { socketProxy = previousProxy })
	for _, test := range []struct {
		name       string
		method     string
		url        string
		inspection string
		body       string
		status     int
		wantErr    bool
		wantPath   string
	}{
		{name: "list", method: "GET", url: "/containers/json"},
		{name: "allowed ID", method: "GET", url: "/v1.54/containers/abc/json", inspection: `{"Id":"abc123","Name":"/db_1"}`, wantPath: "/v1.54/containers/abc123/json"},
		{name: "allowed name pinned to ID", method: "POST", url: "/containers/db_1/stop", inspection: `{"Id":"abc123","Name":"/db_1"}`, wantPath: "/containers/abc123/stop"},
		{name: "forbidden ID", method: "GET", url: "/containers/abc/json", inspection: `{"Id":"abc","Name":"/web"}`, wantErr: true},
		{name: "missing container", method: "GET", url: "/containers/missing/json", status: 404, wantErr: true},
		{name: "bad inspection", method: "GET", url: "/containers/abc/json", inspection: `{`, wantErr: true},
		{name: "prune", method: "POST", url: "/containers/prune", wantErr: true},
		{name: "explicit create", method: "POST", url: "/containers/create?name=db_2"},
		{name: "unnamed create", method: "POST", url: "/containers/create", wantErr: true},
		{name: "forbidden create", method: "POST", url: "/containers/create?name=web", wantErr: true},
		{name: "forbidden rename", method: "POST", url: "/containers/db_1/rename?name=web", wantErr: true},
		{name: "allowed rename", method: "POST", url: "/containers/db_1/rename?name=db_2", inspection: `{"Id":"abc123","Name":"/db_1"}`},
		{name: "noncanonical path", method: "GET", url: "/containers/../containers/abc/json", wantErr: true},
		{name: "double slash", method: "GET", url: "/containers//abc/json", wantErr: true},
		{name: "exec forbidden container", method: "POST", url: "/exec/exec123/start", inspection: `{"ContainerID":"abc","Id":"abc","Name":"/web"}`, wantErr: true},
		{name: "exec allowed container", method: "POST", url: "/exec/exec123/start", inspection: `{"ContainerID":"abc","Id":"abc","Name":"/db_1"}`},
		{name: "commit forbidden container", method: "POST", url: "/commit?container=abc", inspection: `{"Id":"abc","Name":"/web"}`, wantErr: true},
		{name: "network connect forbidden container", method: "POST", url: "/networks/net/connect", body: `{"Container":"abc"}`, inspection: `{"Id":"abc","Name":"/web"}`, wantErr: true},
		{name: "network disconnect forbidden container", method: "POST", url: "/networks/net/disconnect", body: `{"Container":"abc"}`, inspection: `{"Id":"abc","Name":"/web"}`, wantErr: true},
		{name: "network connect allowed container", method: "POST", url: "/networks/net/connect", body: `{"Container":"db_1","container":"web","EndpointConfig":{"Aliases":["alias"]}}`, inspection: `{"Id":"abc123","Name":"/db_1"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			socketProxy = &httputil.ReverseProxy{Transport: inspectionTransport(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/json") {
					t.Fatalf("unexpected inspection: %s %s", request.Method, request.URL)
				}
				status := test.status
				if status == 0 {
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(test.inspection))}, nil
			})}
			request := httptest.NewRequest(test.method, test.url, strings.NewReader(test.body))
			filter := containerFilter{prefixes: []string{"db", "redis"}}
			err := filter.checkRequest(request)
			if (err != nil) != test.wantErr {
				t.Fatalf("checkContainerNameRestrictions() error = %v, wantErr %v", err, test.wantErr)
			}
			if test.wantPath != "" && request.URL.Path != test.wantPath {
				t.Errorf("path = %q, want %q", request.URL.Path, test.wantPath)
			}
			if test.body != "" && err == nil {
				body, readErr := io.ReadAll(request.Body)
				if readErr != nil || string(body) != `{"Container":"abc123","EndpointConfig":{"Aliases":["alias"]}}` {
					t.Errorf("network body = %s, error = %v", body, readErr)
				}
				if int64(len(body)) != request.ContentLength {
					t.Errorf("ContentLength = %d, actual length = %d", request.ContentLength, len(body))
				}
			}
		})
	}
}

func TestFilterContainerNameResponse(t *testing.T) {
	for _, test := range []struct {
		name    string
		path    string
		body    string
		want    string
		wantErr bool
	}{
		{name: "list", path: "/containers/json", body: `[{"Id":"1","Names":["/db_1"],"Extra":42},{"Id":"2","Names":["/web"]},{"Id":"3","Names":["/redis"]}]`, want: `[{"Id":"1","Names":["/db_1"],"Extra":42},{"Id":"3","Names":["/redis"]}]`},
		{name: "empty", path: "/v1.54/containers/json", body: `[{"Names":["/web","/db_1/web"]}]`, want: `[]`},
		{name: "invalid list", path: "/containers/json", body: `{`, wantErr: true},
		{name: "unrelated response", path: "/version", body: `{"Version":"1"}`, want: `{"Version":"1"}`},
		{name: "events", path: "/v1.54/events", body: `{"Type":"container","Actor":{"Attributes":{"name":"web"}}}
{"Type":"network","Actor":{"Attributes":{"name":"db_net"}}}
{"Type":"container","Actor":{"Attributes":{"name":"db_1"}}}`, want: "{\"Type\":\"container\",\"Actor\":{\"Attributes\":{\"name\":\"db_1\"}}}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body)), Request: httptest.NewRequest("GET", test.path, nil)}
			filter := containerFilter{prefixes: []string{"db", "redis"}}
			err := filter.filterResponse(response)
			if (err != nil) != test.wantErr {
				t.Fatalf("filterContainerNameResponse() error = %v", err)
			}
			if err != nil {
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || string(body) != test.want {
				t.Fatalf("body = %s, error = %v; want %s", body, err, test.want)
			}
			if test.path == "/containers/json" && !json.Valid(body) {
				t.Fatal("filtered response is not JSON")
			}
		})
	}
}

func TestHandleHTTPRequestContainerNames(t *testing.T) {
	previousConfig, previousProxy := cfg, socketProxy
	t.Cleanup(func() { cfg, socketProxy = previousConfig, previousProxy })
	for _, test := range []struct {
		name       string
		path       string
		prefixes   []string
		labels     []string
		list       string
		inspection string
		wantStatus int
		wantBody   string
	}{
		{name: "filtered list", path: "/containers/json", prefixes: []string{"db"}, wantStatus: 200, wantBody: `[{"Id":"abc","Names":["/db_1"]}]`},
		{name: "unfiltered list", path: "/containers/json", wantStatus: 200, wantBody: `[{"Id":"abc","Names":["/db_1"]},{"Id":"def","Names":["/web"]}]`},
		{name: "allowed ID", path: "/containers/abc/json", prefixes: []string{"db"}, wantStatus: 200, wantBody: `{"Id":"abc","Name":"/db_1"}`},
		{name: "forbidden ID", path: "/containers/def/json", prefixes: []string{"db"}, wantStatus: 403},
		{name: "label filtered list", path: "/containers/json", labels: []string{"myfoo", "com.bar=fun"}, list: `[{"Names":["/web"],"Labels":{"myfoo":"","com.bar":"fun"}},{"Names":["/db_1"]}]`, wantStatus: 200, wantBody: `[{"Names":["/web"],"Labels":{"myfoo":"","com.bar":"fun"}}]`},
		{name: "label allowed ID", path: "/containers/abc/json", labels: []string{"myfoo"}, inspection: `{"Id":"abc","Name":"/db_1","Config":{"Labels":{"myfoo":""}}}`, wantStatus: 200, wantBody: `{"Id":"abc","Name":"/db_1","Config":{"Labels":{"myfoo":""}}}`},
		{name: "label forbidden ID", path: "/containers/def/json", labels: []string{"myfoo"}, wantStatus: 403},
		{name: "combined list filters", path: "/containers/json", prefixes: []string{"db"}, labels: []string{"myfoo"}, list: `[{"Names":["/web"],"Labels":{"myfoo":""}},{"Names":["/db_1"]}]`, wantStatus: 200, wantBody: `[]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg = &config.Config{
				AllowFrom: []string{"127.0.0.1/32"},
				AllowLists: &config.AllowListRegistry{Default: config.AllowList{
					AllowedRequests:        map[string][]*regexp.Regexp{"GET": {regexp.MustCompile(`.*`)}},
					AllowedContainerNames:  test.prefixes,
					AllowedContainerLabels: test.labels,
				}},
			}
			socketProxy = &httputil.ReverseProxy{
				Director: func(request *http.Request) {},
				Transport: inspectionTransport(func(request *http.Request) (*http.Response, error) {
					body := `{"Id":"def","Name":"/web"}`
					switch request.URL.Path {
					case "/containers/json":
						body = `[{"Id":"abc","Names":["/db_1"]},{"Id":"def","Names":["/web"]}]`
						if test.list != "" {
							body = test.list
						}
					case "/containers/abc/json":
						body = `{"Id":"abc","Name":"/db_1"}`
						if test.inspection != "" {
							body = test.inspection
						}
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Request: request, Body: io.NopCloser(strings.NewReader(body))}, nil
				}),
			}
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.RemoteAddr = "127.0.0.1:12345"
			response := httptest.NewRecorder()
			handleHTTPRequest(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body)
			}
			if test.wantBody != "" && response.Body.String() != test.wantBody {
				t.Errorf("body = %s, want %s", response.Body, test.wantBody)
			}
		})
	}
}
