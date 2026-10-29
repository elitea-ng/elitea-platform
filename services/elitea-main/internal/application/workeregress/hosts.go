package workeregress

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib"
	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

// Bounds of one settings walk.
const (
	maxWalkDepth  = 32
	maxWalkNodes  = 16_384
	maxValueBytes = 2048
)

var errUnreadableSettings = errors.New("the command's toolkit settings are not readable")

// HostsFromSettings returns the hosts of the URL-valued fields of a settings
// document (a toolkit's settings, an MCP server's, a configuration's), at any
// depth: a field whose name is `url`, `endpoint`, `host`, `api_base` or
// `server` (Rally's instance, families/rally/config.rs), or
// ends in `_url`, `_endpoint` or `_host`. A value without a scheme is read as
// https. Only http, https, ws and wss URLs count, and a host the egresslib
// grammar does not accept (a template, a blank) is skipped.
func HostsFromSettings(content []byte) ([]string, error) {
	return hostsFromSettingsOfType("", content)
}

func hostsFromSettingsOfType(toolkitType string, content []byte) ([]string, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, errUnreadableSettings
	}
	walk := hostWalk{toolkitType: toolkitType}
	walk.value(document, 0)
	return walk.hosts, nil
}

// HostsFromAgentInput reads an agent run's frozen toolkit set: the settings
// (and the type's compiled-in hosts, HostsForToolkit) of every entry of the input's `tools` (an ad hoc run) and of its
// application's `version_details.tools` (an application run). Instructions,
// user input, chat history and every other field are never read.
func HostsFromAgentInput(content []byte) ([]string, error) {
	input := &runtimev1.AgentExecutionInputV1{}
	if err := proto.Unmarshal(content, input); err != nil {
		return nil, errors.New("the agent input is not readable")
	}
	var hosts []string
	read := func(raw []byte) error {
		if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
			return nil
		}
		var entries []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return errUnreadableSettings
		}
		for _, entry := range entries {
			var toolkitType string
			if raw, ok := entry["type"]; ok {
				_ = json.Unmarshal(raw, &toolkitType) // a non-string type names no family
			}
			settings, ok := entry["settings"]
			if !ok {
				hosts = append(hosts, ImplicitToolkitHosts(toolkitType)...)
				continue
			}
			found, err := HostsForToolkit(toolkitType, settings)
			if err != nil {
				return err
			}
			hosts = append(hosts, found...)
		}
		return nil
	}
	if err := read(input.GetTools()); err != nil {
		return nil, err
	}
	if application := input.GetApplication(); len(application) != 0 {
		var frozen struct {
			VersionDetails *struct {
				Tools json.RawMessage `json:"tools"`
			} `json:"version_details"`
		}
		if err := json.Unmarshal(application, &frozen); err != nil {
			return nil, errors.New("the frozen application is not readable")
		}
		if frozen.VersionDetails != nil {
			if err := read(frozen.VersionDetails.Tools); err != nil {
				return nil, err
			}
		}
	}
	return hosts, nil
}

// HostsFromToolkitReadInput reads a direct toolkit read's toolkit.
func HostsFromToolkitReadInput(content []byte) ([]string, error) {
	input := &runtimev1.ToolkitExecuteReadInputV1{}
	if err := proto.Unmarshal(content, input); err != nil {
		return nil, errors.New("the direct toolkit input is not readable")
	}
	return HostsForToolkit(input.GetToolkitType(), input.GetToolkit())
}

type hostWalk struct {
	toolkitType string
	nodes       int
	hosts       []string
}

func (w *hostWalk) value(value any, depth int) {
	w.nodes++
	if depth > maxWalkDepth || w.nodes > maxWalkNodes {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if text, ok := child.(string); ok {
				if urlField(key) {
					if host, ok := bareLabelHost(w.toolkitType, key, text); ok {
						w.hosts = append(w.hosts, host)
					} else if host, ok := hostOf(text); ok {
						w.hosts = append(w.hosts, host)
					}
				}
				continue
			}
			w.value(child, depth+1)
		}
	case []any:
		for _, child := range typed {
			w.value(child, depth+1)
		}
	}
}

func urlField(key string) bool {
	key = strings.ToLower(key)
	switch key {
	case "url", "endpoint", "host", "api_base", "server":
		return true
	}
	return strings.HasSuffix(key, "_url") || strings.HasSuffix(key, "_endpoint") || strings.HasSuffix(key, "_host")
}

// hostOf answers the egresslib spelling of a URL's host.
func hostOf(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > maxValueBytes {
		return "", false
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "ws", "wss":
	default:
		return "", false
	}
	host := parsed.Hostname()
	if host == "" || strings.ContainsAny(host, "{}$%") {
		return "", false
	}
	entry, err := egresslib.ParseEntry(host)
	if err != nil {
		return "", false
	}
	return entry.String(), true
}
