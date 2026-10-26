package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

func containerNameAllowed(prefixes []string, name string) bool {
	name = strings.TrimPrefix(name, "/")
	if strings.Contains(name, "/") {
		return false
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func checkContainerNameRestrictions(prefixes []string, request *http.Request) error {
	if len(prefixes) == 0 {
		return nil
	}
	if path.Clean(request.URL.Path) != request.URL.Path || strings.ContainsAny(request.URL.Path, "%\\") {
		return fmt.Errorf("non-canonical API path")
	}
	parts := strings.Split(normalizeAPIPath(request.URL.Path), "/")
	if len(parts) < 3 {
		return nil
	}
	apiPrefix := ""
	if parts[1] != "v0" {
		apiPrefix = "/" + parts[1]
	}
	switch parts[2] {
	case "containers":
		if len(parts) < 4 {
			return fmt.Errorf("unsupported container endpoint")
		}
		switch parts[3] {
		case "json":
			if len(parts) != 4 || request.Method != http.MethodGet {
				return fmt.Errorf("unsupported container list request")
			}
			return nil
		case "create":
			if len(parts) != 4 || !containerNameAllowed(prefixes, request.URL.Query().Get("name")) {
				return fmt.Errorf("container creation requires an allowed explicit name")
			}
			return nil
		case "prune":
			return fmt.Errorf("container pruning is not allowed with name restrictions")
		}
		if len(parts) >= 5 && parts[4] == "rename" && !containerNameAllowed(prefixes, request.URL.Query().Get("name")) {
			return fmt.Errorf("new container name is not allowed")
		}
		id, err := resolveAllowedContainer(prefixes, request, apiPrefix, parts[3])
		if err != nil {
			return err
		}
		parts[3] = id
		request.URL.Path = apiPrefix + "/" + strings.Join(parts[2:], "/")
		request.URL.RawPath = ""
	case "exec":
		if len(parts) < 4 {
			return fmt.Errorf("missing exec ID")
		}
		var execution struct{ ContainerID string }
		if err := inspectDockerResource(request, apiPrefix+"/exec/"+url.PathEscape(parts[3])+"/json", &execution); err != nil {
			return err
		}
		_, err := resolveAllowedContainer(prefixes, request, apiPrefix, execution.ContainerID)
		return err
	case "commit":
		query := request.URL.Query()
		id, err := resolveAllowedContainer(prefixes, request, apiPrefix, query.Get("container"))
		if err != nil {
			return err
		}
		query.Set("container", id)
		request.URL.RawQuery = query.Encode()
	case "networks":
		if len(parts) != 5 || (parts[4] != "connect" && parts[4] != "disconnect") {
			return nil
		}
		body, err := readAndRestoreBody(request)
		if err != nil {
			return err
		}
		var connection map[string]json.RawMessage
		if err := json.Unmarshal(body, &connection); err != nil {
			return err
		}
		var reference string
		if err := json.Unmarshal(connection["Container"], &reference); err != nil {
			return err
		}
		id, err := resolveAllowedContainer(prefixes, request, apiPrefix, reference)
		if err != nil {
			return err
		}
		for key := range connection {
			if strings.EqualFold(key, "Container") {
				delete(connection, key)
			}
		}
		connection["Container"], err = json.Marshal(id)
		if err != nil {
			return err
		}
		body, err = json.Marshal(connection)
		if err != nil {
			return err
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
	}
	return nil
}

func resolveAllowedContainer(prefixes []string, request *http.Request, apiPrefix, reference string) (string, error) {
	if reference == "" {
		return "", fmt.Errorf("missing container reference")
	}
	var container struct {
		ID   string `json:"Id"`
		Name string
	}
	if err := inspectDockerResource(request, apiPrefix+"/containers/"+url.PathEscape(reference)+"/json", &container); err != nil {
		return "", err
	}
	if !containerNameAllowed(prefixes, container.Name) || container.ID == "" || strings.ContainsAny(container.ID, "/%\\") {
		return "", fmt.Errorf("container name is not allowed")
	}
	return container.ID, nil
}

func inspectDockerResource(request *http.Request, resourcePath string, result any) error {
	inspection, err := http.NewRequestWithContext(request.Context(), http.MethodGet, "http://localhost", nil)
	if err != nil {
		return err
	}
	inspection.URL.Path, err = url.PathUnescape(resourcePath)
	if err != nil {
		return err
	}
	inspection.URL.RawPath = resourcePath
	transport := socketProxy.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	response, err := transport.RoundTrip(inspection)
	if err != nil {
		return fmt.Errorf("cannot inspect Docker resource: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("cannot inspect Docker resource: status %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		return fmt.Errorf("cannot decode Docker inspection: %w", err)
	}
	return nil
}

func filterContainerNameResponse(prefixes []string, response *http.Response) error {
	if response.StatusCode != http.StatusOK {
		return nil
	}
	apiPath := normalizeAPIPath(response.Request.URL.Path)
	parts := strings.Split(apiPath, "/")
	if len(parts) != 4 || parts[2] != "containers" || parts[3] != "json" {
		if len(parts) == 3 && parts[2] == "events" {
			response.Body = &containerEventReader{body: response.Body, decoder: json.NewDecoder(response.Body), prefixes: prefixes}
			response.ContentLength = -1
			response.Header.Del("Content-Length")
		}
		return nil
	}
	var containers []json.RawMessage
	defer response.Body.Close()
	err := json.NewDecoder(response.Body).Decode(&containers)
	if err != nil {
		return fmt.Errorf("cannot decode container list: %w", err)
	}
	filtered := make([]json.RawMessage, 0, len(containers))
	for _, raw := range containers {
		var container struct{ Names []string }
		if err := json.Unmarshal(raw, &container); err != nil {
			return err
		}
		for _, name := range container.Names {
			if containerNameAllowed(prefixes, name) {
				filtered = append(filtered, raw)
				break
			}
		}
	}
	body, err := json.Marshal(filtered)
	if err != nil {
		return err
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

type containerEventReader struct {
	body     io.ReadCloser
	decoder  *json.Decoder
	prefixes []string
	pending  []byte
}

func (reader *containerEventReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	for len(reader.pending) == 0 {
		var raw json.RawMessage
		if err := reader.decoder.Decode(&raw); err != nil {
			return 0, err
		}
		var event struct {
			Type  string
			Actor struct{ Attributes map[string]string }
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return 0, err
		}
		if event.Type == "container" && containerNameAllowed(reader.prefixes, event.Actor.Attributes["name"]) {
			reader.pending = append(raw, '\n')
		}
	}
	count := copy(buffer, reader.pending)
	reader.pending = reader.pending[count:]
	return count, nil
}

func (reader *containerEventReader) Close() error {
	return reader.body.Close()
}
