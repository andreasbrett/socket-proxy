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
			err := checkContainerNameRestrictions([]string{"db", "redis"}, request)
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
			err := filterContainerNameResponse([]string{"db", "redis"}, response)
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
		wantStatus int
		wantBody   string
	}{
		{name: "filtered list", path: "/containers/json", prefixes: []string{"db"}, wantStatus: 200, wantBody: `[{"Id":"abc","Names":["/db_1"]}]`},
		{name: "unfiltered list", path: "/containers/json", wantStatus: 200, wantBody: `[{"Id":"abc","Names":["/db_1"]},{"Id":"def","Names":["/web"]}]`},
		{name: "allowed ID", path: "/containers/abc/json", prefixes: []string{"db"}, wantStatus: 200, wantBody: `{"Id":"abc","Name":"/db_1"}`},
		{name: "forbidden ID", path: "/containers/def/json", prefixes: []string{"db"}, wantStatus: 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg = &config.Config{
				AllowFrom: []string{"127.0.0.1/32"},
				AllowLists: &config.AllowListRegistry{Default: config.AllowList{
					AllowedRequests:       map[string][]*regexp.Regexp{"GET": {regexp.MustCompile(`.*`)}},
					AllowedContainerNames: test.prefixes,
				}},
			}
			socketProxy = &httputil.ReverseProxy{
				Director: func(request *http.Request) {},
				Transport: inspectionTransport(func(request *http.Request) (*http.Response, error) {
					body := `{"Id":"def","Name":"/web"}`
					switch request.URL.Path {
					case "/containers/json":
						body = `[{"Id":"abc","Names":["/db_1"]},{"Id":"def","Names":["/web"]}]`
					case "/containers/abc/json":
						body = `{"Id":"abc","Name":"/db_1"}`
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
