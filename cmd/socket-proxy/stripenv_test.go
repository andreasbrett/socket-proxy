package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wollomatic/socket-proxy/internal/config"
)

func TestHandleHTTPRequestStripEnv(t *testing.T) {
	const original = `{"Config":{"Env":["SECRET=value"],"Image":"test"}}`
	const stripped = `{"Config":{"Env":[],"Image":"test"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, original)
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	previousConfig, previousProxy := cfg, socketProxy
	t.Cleanup(func() { cfg, socketProxy = previousConfig, previousProxy })
	socketProxy = httputil.NewSingleHostReverseProxy(target)
	socketProxy.ModifyResponse = stripEnvIfEnabled
	for _, test := range []struct {
		name   string
		global bool
		label  *bool
		want   string
	}{
		{name: "disabled by default", want: original},
		{name: "globally enabled", global: true, want: stripped},
		{name: "label enables", label: new(true), want: stripped},
		{name: "label disables global", global: true, label: new(false), want: original},
		{name: "label keeps disabled", label: new(false), want: original},
		{name: "disabled again without sticky stripping", want: original},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg = &config.Config{
				StripEnv:  test.global,
				AllowFrom: []string{"127.0.0.1/32"},
				AllowLists: &config.AllowListRegistry{Default: config.AllowList{
					AllowedRequests: map[string][]*regexp.Regexp{http.MethodGet: {regexp.MustCompile(`^/containers/test/json$`)}},
					StripEnv:        test.label,
				}},
			}
			request := httptest.NewRequest(http.MethodGet, "/containers/test/json", nil)
			request.RemoteAddr = "127.0.0.1:12345"
			result := httptest.NewRecorder()
			handleHTTPRequest(result, request)
			if result.Code != http.StatusOK || result.Body.String() != test.want {
				t.Fatalf("response = %d %s, want 200 %s", result.Code, result.Body.String(), test.want)
			}
		})
	}
}

func TestStripEnvResponse(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
		want string
	}{
		{"container inspect", "/v1.54/containers/test/json", `{"Config":{"Env":["SECRET=value"],"Image":"test"},"Size":9007199254740993}`, `{"Config":{"Env":[],"Image":"test"},"Size":9007199254740993}`},
		{"image inspect", "/images/test/json", `{"Config":{"Env":["SECRET=value"]},"ContainerConfig":{"Env":["OTHER=value"]}}`, `{"Config":{"Env":[]},"ContainerConfig":{"Env":[]}}`},
		{"service list", "/services", `[{"Spec":{"TaskTemplate":{"ContainerSpec":{"Env":["SECRET=value"],"Image":"test"}}}}]`, `[{"Spec":{"TaskTemplate":{"ContainerSpec":{"Env":[],"Image":"test"}}}}]`},
		{"empty and alternate case", "/containers/test/json", `{"Config":{"Env":null,"env":[],"ENV":["SECRET=value"]}}`, `{"Config":{"ENV":[],"Env":[],"env":[]}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{
				StatusCode: http.StatusOK,
				Request:    httptest.NewRequest(http.MethodGet, test.path, nil),
				Header:     http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Etag": {"original"}},
				Body:       io.NopCloser(strings.NewReader(test.body)),
			}
			if err := stripEnvResponse(response); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != test.want {
				t.Fatalf("body = %s, want %s", body, test.want)
			}
			if response.ContentLength != int64(len(body)) || response.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
				t.Fatal("incorrect content length")
			}
			if response.Header.Get("ETag") != "" {
				t.Fatal("stale ETag retained")
			}
		})
	}
}

func TestStripEnvResponseFailsClosed(t *testing.T) {
	for _, body := range []string{`{"Config":{"Env":["SECRET=value"]}`, `{} {"Env":["SECRET=value"]}`, ""} {
		t.Run(body, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, body)
			}))
			defer upstream.Close()
			target, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(target)
			proxy.ModifyResponse = stripEnvResponse
			result := httptest.NewRecorder()
			proxy.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/containers/test/json", nil))
			if result.Code != http.StatusBadGateway || strings.Contains(result.Body.String(), "SECRET") {
				t.Fatalf("unexpected response: %d %s", result.Code, result.Body.String())
			}
		})
	}
}

func TestStripEnvResponsePassThrough(t *testing.T) {
	for _, path := range []string{"/events", "/v1.54/events", "/containers/test/stats?stream=false", "/v1.54/containers/test/logs", "/services/test/logs", "/tasks/test/logs", "/build", "/images/create", "/images/load", "/images/test/push", "/v1.54/images/registry.example.com/team/image/push", "/plugins/pull", "/exec/test/start"} {
		t.Run(path, func(t *testing.T) {
			body := io.NopCloser(strings.NewReader("stream"))
			response := &http.Response{
				StatusCode: http.StatusOK,
				Request:    httptest.NewRequest(http.MethodGet, path, nil),
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       body,
			}
			if err := stripEnvResponse(response); err != nil {
				t.Fatal(err)
			}
			if response.Body != body {
				t.Fatal("stream body was replaced")
			}
		})
	}
}

func TestStripEnvResponseUnmodifiedResponses(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		status      int
		contentType string
	}{
		{"plain text", http.MethodGet, http.StatusOK, "text/plain"},
		{"archive", http.MethodGet, http.StatusOK, "application/x-tar"},
		{"head", http.MethodHead, http.StatusOK, "application/json"},
		{"upgrade", http.MethodGet, http.StatusSwitchingProtocols, "application/json"},
		{"no content", http.MethodGet, http.StatusNoContent, "application/json"},
		{"not modified", http.MethodGet, http.StatusNotModified, "application/json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := io.NopCloser(strings.NewReader("unchanged"))
			response := &http.Response{
				StatusCode: test.status,
				Request:    httptest.NewRequest(test.method, "/containers/test/json", nil),
				Header:     http.Header{"Content-Type": {test.contentType}},
				Body:       body,
			}
			if err := stripEnvResponse(response); err != nil {
				t.Fatal(err)
			}
			if response.Body != body {
				t.Fatal("body was replaced")
			}
		})
	}
}

func TestStripEnvResponseRejectsEncoding(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Request:    httptest.NewRequest(http.MethodGet, "/containers/test/json", nil),
		Header:     http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}},
		Body:       io.NopCloser(strings.NewReader("compressed")),
	}
	if err := stripEnvResponse(response); err == nil {
		t.Fatal("encoded response was not rejected")
	}
}
