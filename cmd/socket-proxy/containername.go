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

type containerFilter struct {
	prefixes []string
	labels   []string
}

func (filter containerFilter) enabled() bool {
	return len(filter.prefixes) > 0 || len(filter.labels) > 0
}

func (filter containerFilter) labelsAllowed(labels map[string]string) bool {
	for _, requirement := range filter.labels {
		key, expected, requireValue := strings.Cut(requirement, "=")
		value, exists := labels[key]
		if !exists || (requireValue && value != expected) {
			return false
		}
	}
	return true
}

func (filter containerFilter) nameAllowed(name string) bool {
	return len(filter.prefixes) == 0 || containerNameAllowed(filter.prefixes, name)
}

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

func (filter containerFilter) checkRequest(request *http.Request) error {
	if !filter.enabled() {
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
		return filter.checkContainerRequest(request, apiPrefix, parts[3:])
	case "exec":
		return filter.checkExecRequest(request, apiPrefix, parts[3:])
	case "commit":
		return filter.checkCommitRequest(request, apiPrefix)
	case "networks":
		return filter.checkNetworkRequest(request, apiPrefix, parts[3:])
	}
	return nil
}

func (filter containerFilter) checkContainerRequest(request *http.Request, apiPrefix string, resource []string) error {
	if len(resource) == 0 {
		return fmt.Errorf("unsupported container endpoint")
	}
	switch resource[0] {
	case "json":
		if len(resource) != 1 || request.Method != http.MethodGet {
			return fmt.Errorf("unsupported container list request")
		}
		return nil
	case "create":
		if len(resource) != 1 {
			return fmt.Errorf("container creation requires an allowed explicit name")
		}
		return filter.checkContainerCreation(request)
	case "prune":
		return fmt.Errorf("container pruning is not allowed with container restrictions")
	}
	if len(resource) >= 2 && resource[1] == "rename" && !filter.nameAllowed(request.URL.Query().Get("name")) {
		return fmt.Errorf("new container name is not allowed")
	}
	id, err := filter.resolveAllowedContainer(request, apiPrefix, resource[0])
	if err != nil {
		return err
	}
	resource[0] = id
	request.URL.Path = apiPrefix + "/containers/" + strings.Join(resource, "/")
	request.URL.RawPath = ""
	return nil
}

func (filter containerFilter) checkContainerCreation(request *http.Request) error {
	if !filter.nameAllowed(request.URL.Query().Get("name")) {
		return fmt.Errorf("container creation requires an allowed explicit name")
	}
	if len(filter.labels) == 0 {
		return nil
	}
	body, err := readAndRestoreBody(request)
	if err != nil {
		return err
	}
	var creation struct{ Labels map[string]string }
	if err := json.Unmarshal(body, &creation); err != nil {
		return err
	}
	if !filter.labelsAllowed(creation.Labels) {
		return fmt.Errorf("container creation requires matching explicit labels")
	}
	return nil
}

func (filter containerFilter) checkExecRequest(request *http.Request, apiPrefix string, resource []string) error {
	if len(resource) == 0 {
		return fmt.Errorf("missing exec ID")
	}
	var execution struct{ ContainerID string }
	if err := inspectDockerResource(request, apiPrefix+"/exec/"+url.PathEscape(resource[0])+"/json", &execution); err != nil {
		return err
	}
	_, err := filter.resolveAllowedContainer(request, apiPrefix, execution.ContainerID)
	return err
}

func (filter containerFilter) checkCommitRequest(request *http.Request, apiPrefix string) error {
	query := request.URL.Query()
	id, err := filter.resolveAllowedContainer(request, apiPrefix, query.Get("container"))
	if err != nil {
		return err
	}
	query.Set("container", id)
	request.URL.RawQuery = query.Encode()
	return nil
}

func (filter containerFilter) checkNetworkRequest(request *http.Request, apiPrefix string, resource []string) error {
	if len(resource) != 2 || (resource[1] != "connect" && resource[1] != "disconnect") {
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
	id, err := filter.resolveAllowedContainer(request, apiPrefix, reference)
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
	return nil
}

func (filter containerFilter) resolveAllowedContainer(request *http.Request, apiPrefix, reference string) (string, error) {
	if reference == "" {
		return "", fmt.Errorf("missing container reference")
	}
	var container struct {
		ID     string `json:"Id"`
		Name   string
		Config struct{ Labels map[string]string }
	}
	if err := inspectDockerResource(request, apiPrefix+"/containers/"+url.PathEscape(reference)+"/json", &container); err != nil {
		return "", err
	}
	if !filter.nameAllowed(container.Name) || !filter.labelsAllowed(container.Config.Labels) || container.ID == "" || strings.ContainsAny(container.ID, "/%\\") {
		return "", fmt.Errorf("container name or labels are not allowed")
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

func (filter containerFilter) filterResponse(response *http.Response) error {
	if response.StatusCode != http.StatusOK {
		return nil
	}
	apiPath := normalizeAPIPath(response.Request.URL.Path)
	parts := strings.Split(apiPath, "/")
	if len(parts) != 4 || parts[2] != "containers" || parts[3] != "json" {
		if len(parts) == 3 && parts[2] == "events" {
			apiPrefix := ""
			if parts[1] != "v0" {
				apiPrefix = "/" + parts[1]
			}
			response.Body = &containerEventReader{body: response.Body, decoder: json.NewDecoder(response.Body), filter: filter, request: response.Request, apiPrefix: apiPrefix}
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
		var container struct {
			Names  []string
			Labels map[string]string
		}
		if err := json.Unmarshal(raw, &container); err != nil {
			return err
		}
		if !filter.labelsAllowed(container.Labels) {
			continue
		}
		if len(filter.prefixes) == 0 {
			filtered = append(filtered, raw)
			continue
		}
		for _, name := range container.Names {
			if filter.nameAllowed(name) {
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
	body      io.ReadCloser
	decoder   *json.Decoder
	filter    containerFilter
	request   *http.Request
	apiPrefix string
	pending   []byte
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
			Actor struct {
				ID         string
				Attributes map[string]string
			}
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return 0, err
		}
		if event.Type != "container" || !reader.filter.nameAllowed(event.Actor.Attributes["name"]) {
			continue
		}
		if len(reader.filter.labels) > 0 {
			if _, err := reader.filter.resolveAllowedContainer(reader.request, reader.apiPrefix, event.Actor.ID); err != nil {
				continue
			}
		}
		reader.pending = append(raw, '\n')
	}
	count := copy(buffer, reader.pending)
	reader.pending = reader.pending[count:]
	return count, nil
}

func (reader *containerEventReader) Close() error {
	return reader.body.Close()
}
