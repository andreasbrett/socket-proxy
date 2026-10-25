package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var streamingResponsePath = regexp.MustCompile(`^/(v[0-9]+(\.[0-9]+)?/)?(events|build|containers/[^/]+/(attach(/ws)?|logs|stats)|services/[^/]+/logs|tasks/[^/]+/logs|exec/[^/]+/start|images/(create|load|.+/push)|plugins/pull)/?$`)

type stripEnvContextKey struct{}

func stripEnvIfEnabled(response *http.Response) error {
	if enabled, _ := response.Request.Context().Value(stripEnvContextKey{}).(bool); !enabled {
		return nil
	}
	return stripEnvResponse(response)
}

func stripEnvResponse(response *http.Response) error {
	if response.Request.Method == http.MethodHead || response.StatusCode == http.StatusSwitchingProtocols || streamingResponsePath.MatchString(response.Request.URL.Path) {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusNotModified {
		return nil
	}
	defer response.Body.Close()
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return fmt.Errorf("cannot perform environment variable stripping on encoded JSON response")
	}
	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("cannot decode JSON response for environment variable stripping")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("expected a single JSON response for environment variable stripping")
	}
	stripEnv(document)
	body, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("cannot encode JSON response after environment variable stripping")
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	response.Header.Del("ETag")
	response.Header.Del("Content-MD5")
	response.Header.Del("Digest")
	return nil
}

func stripEnv(value any) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if strings.EqualFold(key, "Env") {
				value[key] = []any{}
			} else {
				stripEnv(child)
			}
		}
	case []any:
		for _, child := range value {
			stripEnv(child)
		}
	}
}
