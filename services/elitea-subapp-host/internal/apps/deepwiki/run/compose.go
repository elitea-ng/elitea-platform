package run

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// DefaultBucket is the bucket every artifact object names at invoke time —
// "wiki-artifacts", which disagrees with the descriptor's "wiki". Legacy
// behaviour; the invoke-time value is the one that reached the platform, so
// it is the one preserved. A test pins the disagreement.
const DefaultBucket = "wiki-artifacts"

// Object is one composed result object. A message carries the first four
// fields; an artifact carries all seven. Field order matches the legacy
// dicts so the encoded list reads as the recorded one.
type Object struct {
	Name            *string `json:"name,omitempty"`
	ObjectType      string  `json:"object_type"`
	ResultTarget    string  `json:"result_target"`
	ResultExtension string  `json:"result_extension,omitempty"`
	ResultEncoding  string  `json:"result_encoding"`
	ResultBucket    string  `json:"result_bucket,omitempty"`
	Data            string  `json:"data"`
}

// IsArtifact reports whether the object is bound for a bucket.
func (o Object) IsArtifact() bool { return o.ResultTarget == "artifact" }

// NameString is the artifact's key, "" for a message or a nameless artifact.
func (o Object) NameString() string {
	if o.Name == nil {
		return ""
	}
	return *o.Name
}

// Message is a response message object.
func Message(data string) Object {
	return Object{ObjectType: "message", ResultTarget: "response", ResultEncoding: "plain", Data: data}
}

// Response is a message object under another object_type. The wiki_query
// family answered `wiki_list`, `answer`, `report` and `message` — the same
// four fields in the same order, only the type differing — and an agent
// reading the result list branches on it.
func Response(objectType, data string) Object {
	return Object{ObjectType: objectType, ResultTarget: "response", ResultEncoding: "plain", Data: data}
}

// Artifact is a bucket-bound object. A nil name is the legacy `None`.
func Artifact(name *string, objectType, extension, data string) Object {
	return Object{Name: name, ObjectType: objectType, ResultTarget: "artifact", ResultExtension: extension,
		ResultEncoding: "plain", ResultBucket: DefaultBucket, Data: data}
}

func ptr(s string) *string { return &s }

// classifyJSONArtifact is the legacy branch verbatim, sniff included: a name
// containing "wiki_manifest_" is a manifest; otherwise a body that parses as
// an object with a truthy wiki_version_id and a pages LIST is also a
// manifest, and anything else is the structure. A nameless artifact gets a
// synthesised name. The sniff exists because the worker could emit a
// manifest with no name at all.
func classifyJSONArtifact(artifact map[string]any) (objectType, name string) {
	name = str(artifact["name"])
	objectType = "wiki_structure"
	var manifestVersion string
	if strings.Contains(name, "wiki_manifest_") {
		objectType = "wiki_manifest"
	} else if data := str(artifact["data"]); strings.HasPrefix(strings.TrimSpace(data), "{") {
		var parsed map[string]any
		if json.Unmarshal([]byte(data), &parsed) == nil {
			_, pagesIsList := parsed["pages"].([]any)
			if Truthy(parsed["wiki_version_id"]) && pagesIsList {
				objectType = "wiki_manifest"
				manifestVersion = fmt.Sprint(parsed["wiki_version_id"])
			}
		}
	}
	if strings.TrimSpace(name) == "" {
		if objectType == "wiki_manifest" {
			if manifestVersion == "" {
				manifestVersion = "unknown"
			}
			name = "wiki_manifest_" + manifestVersion + ".json"
		} else {
			name = "wiki_structure.json"
		}
	}
	return objectType, name
}

func partialFailureMessages(result map[string]any) []Object {
	errorsList, _ := result["errors"].([]any)
	failedPages, _ := result["failed_pages"].([]any)
	if len(errorsList) == 0 && len(failedPages) == 0 {
		return nil
	}
	var objects []Object
	var summary []string
	if len(failedPages) > 0 {
		summary = append(summary, fmt.Sprintf("Failed pages: %d", len(failedPages)))
	}
	if len(errorsList) > 0 {
		summary = append(summary, fmt.Sprintf("Errors: %d", len(errorsList)))
	}
	objects = append(objects, Message("⚠️ Partial issues detected:\n"+strings.Join(summary, "\n")))
	if len(failedPages) > 0 {
		var lines []string
		for _, item := range failedPages {
			if page := object(item); page != nil {
				pageID := firstTruthy(page["page_id"], "(unknown)")
				title := firstTruthy(page["title"], "")
				status := firstTruthy(page["status"], "")
				lines = append(lines, strings.TrimSpace(fmt.Sprintf("- %v %v (%v)", pageID, title, status)))
			} else {
				lines = append(lines, fmt.Sprintf("- %v", item))
			}
		}
		objects = append(objects, Message("Failed pages:\n"+strings.Join(lines, "\n")))
	}
	if len(errorsList) > 0 {
		lines := make([]string, 0, len(errorsList))
		for _, e := range errorsList {
			lines = append(lines, fmt.Sprintf("- %v", e))
		}
		objects = append(objects, Message("Errors:\n"+strings.Join(lines, "\n")))
	}
	return objects
}

// stringOr is Python's dict.get(key, default) coerced to a string.
func stringOr(result map[string]any, key, fallback string) string {
	if v, ok := result[key]; ok {
		if s, isString := v.(string); isString {
			return s
		}
		return fmt.Sprint(v)
	}
	return fallback
}

// ComposeResultObjects is the legacy composer: the response message first,
// partial failures, then one object per artifact and — for generate_wiki —
// the repository context. Pinned by composed_result.json.
func ComposeResultObjects(tool string, result map[string]any) []Object {
	var objects []Object
	// The wiki_query family composes to exactly ONE response object and
	// nothing else — no partial-failure messages, no artifacts, no
	// repository context. None of those tools produces an artifact, and the
	// legacy handlers wrote their one-object list themselves.
	switch tool {
	case "list_wikis", "resolve_and_ask", "resolve_and_deep_research", "delete_wiki":
		return []Object{Response(stringOr(result, "object_type", "message"), str(result["data"]))}
	}
	switch tool {
	case "ask":
		objects = append(objects, Message(stringOr(result, "answer", "Question answered successfully")))
		if sources, _ := result["sources"].([]any); len(sources) > 0 {
			if len(sources) > 5 {
				sources = sources[:5]
			}
			lines := make([]string, 0, len(sources))
			for _, source := range sources {
				lines = append(lines, "- "+stringOr(object(source), "source", "unknown"))
			}
			objects = append(objects, Message("\n\nSources:\n"+strings.Join(lines, "\n")))
		}
	case "deep_research":
		objects = append(objects, Message(stringOr(result, "report", stringOr(result, "answer", "Deep research completed successfully"))))
	default:
		objects = append(objects, Message(stringOr(result, "result", "Wiki generation completed successfully")))
	}
	objects = append(objects, partialFailureMessages(result)...)

	artifacts, _ := result["artifacts"].([]any)
	for _, raw := range artifacts {
		artifact := object(raw)
		if artifact == nil || Truthy(artifact["_uploaded_directly"]) {
			continue
		}
		switch str(artifact["type"]) {
		case "application/json":
			objectType, name := classifyJSONArtifact(artifact)
			objects = append(objects, Artifact(ptr(name), objectType, "json", str(artifact["data"])))
		case "text/markdown":
			var name *string
			if s, ok := artifact["name"].(string); ok {
				name = ptr(s)
			}
			objects = append(objects, Artifact(name, "wiki_page", "md", str(artifact["data"])))
		}
	}
	if tool == "generate_wiki" {
		if context := str(result["repository_context"]); context != "" {
			name := "repository_context.txt"
			if wikiID := str(result["wiki_id"]); wikiID != "" {
				name = wikiID + "/repository_context.txt"
			}
			objects = append(objects, Artifact(ptr(name), "repository_context", "txt", context))
		}
	}
	return objects
}

// CheckWikiHasPages refuses a generate_wiki result whose manifest lists no
// pages. Measured 2026-09-02 on PR #725: the engine's analysis call answered
// 404 for an unconfigured model, and the worker still printed "[worker] Done"
// and returned a manifest whose `pages` list was empty — so this host
// uploaded that manifest and marked the invocation Completed, and the wiki
// browser showed a finished generation with nothing in it. A run that
// produced no pages is the engine's inference failing, and it has to be told
// as one before anything reaches the bucket: an empty manifest that lands is
// indistinguishable from a real wiki.
func CheckWikiHasPages(tool string, objects []Object, result map[string]any) error {
	if tool != "generate_wiki" {
		return nil
	}
	for _, obj := range objects {
		if !obj.IsArtifact() || obj.ObjectType != "wiki_manifest" {
			continue
		}
		var manifest map[string]any
		if json.Unmarshal([]byte(obj.Data), &manifest) != nil {
			// A body this guard cannot read is not evidence of an empty
			// wiki, so such a run keeps the behaviour it already had.
			continue
		}
		if pages, _ := manifest["pages"].([]any); len(pages) > 0 {
			continue
		}
		// The wording is load-bearing: spi.Classify sends a runtime failure
		// whose text carries "generat" to inference_failed, which is what
		// this is — the engine's, not this host's.
		message := fmt.Sprintf("wiki generation produced no pages: the engine reported success, but the manifest %s lists none, so nothing was stored", obj.NameString())
		if log := LastWorkerLog(result); log != "" {
			message += "\n" + log
		}
		return spi.Failf(spi.KindRuntime, "%s", message)
	}
	return nil
}

// LastWorkerLog is the engine's own "Last worker log: …" line, which it
// appends to the error of a failed result. A result that claims success
// carries no error, so this reads `errors` as well; "" when there is none.
func LastWorkerLog(result map[string]any) string {
	candidates := []string{str(result["error"])}
	if errorsList, _ := result["errors"].([]any); len(errorsList) > 0 {
		for _, e := range errorsList {
			candidates = append(candidates, fmt.Sprint(e))
		}
	}
	for i := len(candidates) - 1; i >= 0; i-- {
		for _, line := range strings.Split(candidates[i], "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "Last worker log:") {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

// CompletedBody is the terminal invocation body: result is a JSON STRING
// holding the object list, and result_type is always "String".
func CompletedBody(invocationID string, objects []Object) map[string]any {
	if objects == nil {
		objects = []Object{}
	}
	encoded, _ := json.Marshal(objects)
	return map[string]any{
		"invocation_id": invocationID,
		"status":        "Completed",
		"result":        string(encoded),
		"result_type":   "String",
	}
}
