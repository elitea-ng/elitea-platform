package run

// Reader-selected wiki pages as question context (`context_paths`).
//
// WHAT THIS IS. The wiki chat can be given ATTACHMENTS: a reader picks pages
// out of the wiki they are reading and asks a question against exactly those,
// instead of against whatever retrieval happens to surface. New product, not
// a port — legacy DeepWiki had nothing like it.
//
// THE MECHANISM. The client sends IDENTIFIERS — page ids, never text and
// never URLs. This resolves them against the wiki's OWN bucket, caps the
// total, and PREPENDS a rendered block to `question` before the tool is
// dispatched. Prepending is the engine's own idiom (engine/ask_tool.py's
// `enhanced_question`), so there is one convention for "extra context in
// front of the question" rather than two.
//
// WHY IT IS RESOLVED HERE, in front of the tool table, rather than inside
// one tool. Every runner on this host goes through Runner.Invoke: the
// fixture table the browser journeys run against, and the sidecar table
// that reaches the real Python engine. Resolving here means the feature is
// the same on both, and the one place a client-supplied identifier can turn
// into a read is this function. The keys are then REMOVED from the argument
// set (see consumeContextParams), so the Python wrapper's own resolver —
// which serves the standalone stack, where this host is not in the path —
// cannot prepend the same context a second time.
//
// SECURITY. The wiki id is DERIVED from repo_config, exactly as the rest of
// the invocation derives it, so a client can only attach pages of the wiki
// it is already asking about; the version is pinned by the caller and the
// manifest read is for that exact version; and an id that is not published
// in that manifest is REFUSED, not dropped. A dropped attachment is an
// answer that looks grounded in pages it never saw.
//
// TWO IMPLEMENTATIONS, ONE FIXTURE. The Python wrapper
// (services/elitea-deepwiki/src/elitea_deepwiki/wiki_context.py) carries the
// same rules for the standalone stack. Both are pinned to
// conformance/provider/fixtures/deepwiki/context/context_paths.json, so the
// two cannot drift without a test saying so.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// The budgets, in characters. Total across the block, and per document.
const (
	TotalBudgetChars        = 32000
	PerDocumentBudgetChars  = 8000
	ContextLeadIn           = "Given this selected wiki context:"
	QuestionLeadIn          = "Current question: "
	TruncationMarkerFormat  = "\n\n[… truncated to %d characters of context budget]"
	ContextPathsParam       = "context_paths"
	ContextVersionParam     = "context_wiki_version_id"
	contextOmittedFormat    = "\n\n[… %d further selected page(s) omitted for the %d character context budget: %s]"
	contextSectionSeparator = "\n\n"
)

// ContextTools are the tools a selection may be attached to. generate_wiki
// has no question to prepend to, and the wiki_query family resolves WHICH
// wiki only after the model has answered — a selection made before that is
// a selection against an unknown wiki, so it is not offered there.
var ContextTools = map[string]bool{"ask": true, "deep_research": true}

// pageID is a page id's permitted shape: a SECOND gate, not the only one —
// the id must also appear in the pinned manifest. It exists so a malformed
// or hostile id never reaches the artifact client at all. No scheme, no
// absolute path, no traversal, no empty segment, and it must be a wiki page.
var pageID = regexp.MustCompile(`^wiki_pages(?:/[A-Za-z0-9._][A-Za-z0-9._-]*)+\.md$`)

// versionID is part of an object key, so it is held to the same rule.
var versionID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// ManifestKey is the pinned version's manifest object key.
func ManifestKey(wikiID, wikiVersionID string) string {
	return wikiID + "/wiki_manifest_" + wikiVersionID + ".json"
}

// NormalisePageID strips a leading `{wikiID}/`.
//
// BOTH FORMS EXIST IN THE WILD: the recorded legacy manifest lists pages as
// FULL keys, the fixture runners list them RELATIVE. A resolver that took
// only one would refuse every attachment on the other stack, which is the
// drift the two-fixture-runner rule exists to catch. Both sides of every
// comparison go through here.
func NormalisePageID(wikiID, raw string) string {
	return strings.TrimPrefix(strings.TrimSpace(raw), wikiID+"/")
}

// contextSelection is the requested ids, order preserved, duplicates
// collapsed. A non-list, or a list holding anything but a non-empty string,
// is refused rather than coerced.
func contextSelection(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	entries, ok := value.([]any)
	if !ok {
		return nil, spi.Failf(spi.KindValue, "%s must be a list of wiki page ids", ContextPathsParam)
	}
	seen := map[string]bool{}
	ordered := make([]string, 0, len(entries))
	for _, entry := range entries {
		text, ok := entry.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, spi.Failf(spi.KindValue,
				"%s must contain wiki page ids as strings; refused %v", ContextPathsParam, entry)
		}
		text = strings.TrimSpace(text)
		if !seen[text] {
			seen[text] = true
			ordered = append(ordered, text)
		}
	}
	return ordered, nil
}

// truncate is a deterministic prefix plus a visible marker naming the limit
// that applied, so a truncated answer is explainable by reading the block.
func truncate(body string, limit int) string {
	runes := []rune(body)
	if len(runes) <= limit {
		return body
	}
	return string(runes[:limit]) + fmt.Sprintf(TruncationMarkerFormat, limit)
}

// BuildContextBlock renders an already-validated selection under the two
// budgets. readPage answers one page's markdown for a normalised id.
func BuildContextBlock(selection []string, readPage func(string) (string, error)) (string, error) {
	sections := make([]string, 0, len(selection))
	var omitted []string
	spent := 0
	for _, pageID := range selection {
		remaining := TotalBudgetChars - spent
		if remaining <= 0 {
			omitted = append(omitted, pageID)
			continue
		}
		body, err := readPage(pageID)
		if err != nil {
			return "", err
		}
		body = truncate(body, PerDocumentBudgetChars)
		if len([]rune(body)) > remaining {
			body = truncate(body, remaining)
		}
		spent += len([]rune(body))
		sections = append(sections, "--- source: "+pageID+" ---\n"+body)
	}
	block := strings.Join(sections, contextSectionSeparator)
	if len(omitted) > 0 {
		// Named, not dropped: an attachment that contributed nothing has to
		// be visible in the transcript.
		block += fmt.Sprintf(contextOmittedFormat, len(omitted), TotalBudgetChars, strings.Join(omitted, ", "))
	}
	return block, nil
}

// PrependContext is the engine's enhanced_question shape, with our lead-in.
func PrependContext(question, block string) string {
	if block == "" {
		return question
	}
	return ContextLeadIn + "\n" + block + "\n\n" + QuestionLeadIn + question
}

// contextStore is the read half of the artifact transport this needs.
type contextStore interface {
	Download(ctx context.Context, bucket, key string) ([]byte, error)
}

// ApplyContextPaths resolves a selection into params["question"] and removes
// the keys it spent. A tool that takes no selection, or a request that
// carries none, is left exactly as it was.
func ApplyContextPaths(ctx context.Context, tool string, params Params, transport ArtifactClientFactory) (Params, error) {
	if !ContextTools[tool] {
		// Refuse loudly rather than ignoring: a selection sent to a tool
		// that cannot honour it would otherwise answer ungrounded and look
		// like the feature working.
		if selection, err := contextSelection(params[ContextPathsParam]); err != nil {
			return nil, err
		} else if len(selection) > 0 {
			return nil, spi.Failf(spi.KindValue,
				"%s is not supported by %s; attach pages to ask or deep_research", ContextPathsParam, tool)
		}
		return consumeContextParams(params), nil
	}

	selection, err := contextSelection(params[ContextPathsParam])
	if err != nil {
		return nil, err
	}
	if len(selection) == 0 {
		return consumeContextParams(params), nil
	}

	version := strings.TrimSpace(str(params[ContextVersionParam]))
	if version == "" {
		return nil, spi.Failf(spi.KindValue,
			"%s is required when %s is given: a selection is pinned to the wiki version it was made in, so it cannot silently resolve against a newer one",
			ContextVersionParam, ContextPathsParam)
	}
	if !versionID.MatchString(version) {
		return nil, spi.Failf(spi.KindValue, "%s is not a wiki version id: %q", ContextVersionParam, version)
	}

	repo := ExtractRepoConfig(params)
	wikiID := WikiIDFor(repo.Map(), repo.BranchString())

	normalised := make([]string, 0, len(selection))
	deduped := map[string]bool{}
	for _, raw := range selection {
		page := NormalisePageID(wikiID, raw)
		// Deduplicated AFTER normalisation, not before: the same page can be
		// named twice in the two id forms, and attaching it twice would
		// charge the reader's budget twice for one page.
		if deduped[page] {
			continue
		}
		deduped[page] = true
		if !pageID.MatchString(page) {
			return nil, spi.Failf(spi.KindValue,
				"%s may only name wiki pages of this wiki; refused %q", ContextPathsParam, raw)
		}
		normalised = append(normalised, page)
	}

	store, refusal := contextTransport(params, transport)
	if store == nil {
		return nil, spi.Failf(spi.KindRuntime, "selected wiki pages cannot be read: %s", refusal)
	}

	raw, err := store.Download(ctx, DefaultBucket, ManifestKey(wikiID, version))
	if err != nil {
		return nil, spi.Failf(spi.KindValue,
			"the wiki version %q is not available, so the selected pages cannot be pinned to it: %v", version, err)
	}
	var manifest struct {
		Pages []string `json:"pages"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, spi.Failf(spi.KindValue, "the manifest for wiki version %q is not readable: %v", version, err)
	}
	published := map[string]bool{}
	for _, entry := range manifest.Pages {
		published[NormalisePageID(wikiID, entry)] = true
	}
	var unknown []string
	for _, page := range normalised {
		if !published[page] {
			unknown = append(unknown, page)
		}
	}
	if len(unknown) > 0 {
		// THE SCOPE REFUSAL. An id that is not in THIS wiki's manifest for
		// THIS version is refused, whether it names another project's wiki,
		// a page that exists only in a newer version, or nothing at all.
		sort.Strings(unknown)
		return nil, spi.Failf(spi.KindValue,
			"selected page(s) are not part of wiki %q version %q: %s", wikiID, version, strings.Join(unknown, ", "))
	}

	block, err := BuildContextBlock(normalised, func(page string) (string, error) {
		body, err := store.Download(ctx, DefaultBucket, wikiID+"/"+page)
		if err != nil {
			return "", spi.Failf(spi.KindValue, "the selected page %q could not be read: %v", page, err)
		}
		return string(body), nil
	})
	if err != nil {
		return nil, err
	}

	next := consumeContextParams(params)
	next["question"] = PrependContext(str(params["question"]), block)
	return next, nil
}

// contextTransport derives the read half for one invocation, or says why
// there is none. Same derivation and the same refusal texts as the
// wiki_query family's store().
func contextTransport(params Params, transport ArtifactClientFactory) (contextStore, string) {
	llmSettings := object(params["llm_settings"])
	if len(llmSettings) == 0 {
		return nil, "artifacts settings not configured"
	}
	if transport == nil {
		return nil, "artifacts base_url not configured"
	}
	client, err := transport(llmSettings)
	if err != nil {
		return nil, fmt.Sprintf("artifacts client error: %v", err)
	}
	if client == nil {
		return nil, "artifacts base_url not configured"
	}
	store, ok := client.(artifacts.Store)
	if !ok {
		return nil, "artifacts client cannot read objects"
	}
	return store, ""
}

// consumeContextParams is params without the keys the resolver has spent.
func consumeContextParams(params Params) Params {
	next := make(Params, len(params))
	for k, v := range params {
		if k == ContextPathsParam || k == ContextVersionParam {
			continue
		}
		next[k] = v
	}
	return next
}
