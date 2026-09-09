// Reader-uploaded text-file attachments as chat context (`extra_context`).
//
// WHAT THIS IS. `contextpaths.go` next door lets a reader attach pages of
// THIS wiki to a question. #873 adds a second attachment kind: small text
// files the reader picked from their own machine — notes, a snippet, a log —
// that have nothing to do with the indexed repository and so cannot be named
// by a `context_paths` identifier at all.
//
// WHY CONTENT, NOT AN IDENTIFIER. `context_paths` sends identifiers because
// the alternative — the client naming what the server reads — is an
// SSRF/arbitrary-read door (see that file's header). A reader's own upload has
// no such door to close: there is nothing on this server for the client to
// name, because the file never touches server storage. The browser reads it
// locally and sends the TEXT, which is exactly what the reader chose to send
// and nothing more.
//
// THE BUDGET is smaller than context_paths' (16k total, 6k per file, 5 files)
// because this is a chat aside, not the wiki's own primary grounding — a
// reader who wants to ground a question in gigabytes of material should be
// asking `ask` against the indexed repository, not pasting it in one turn.
// Truncation is the same deterministic prefix-plus-marker shape.
//
// ORDER. This runs AFTER `ApplyContextPaths` in Runner.Invoke, and prepends
// its OWN block in front of whatever `question` already is — wiki-page
// context first (closest to the question), then the reader's own files
// (furthest from it), because the files are the LEAST authoritative source:
// nothing here validates that they are true, unlike a wiki page resolved
// against the indexed repository. Prepending without its own "Current
// question:" trailer (unlike `PrependContext`) is deliberate: stacking two
// "Current question:" markers when both attachment kinds are used would read
// as if the model were asked the question twice.
package run

import (
	"fmt"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// The budgets, in characters. Total across every attached file, and per file.
const (
	ExtraContextTotalBudgetChars   = 16000
	ExtraContextPerFileBudgetChars = 6000
	// MaxExtraContextFiles bounds the COUNT before any budget math runs, so
	// a caller cannot make the server iterate an arbitrarily long list just
	// to discover every entry after the fifth is truncated to nothing.
	MaxExtraContextFiles = 5

	ExtraContextParam      = "extra_context"
	ExtraContextLeadIn     = "Given these attached files:"
	extraContextTruncation = "\n\n[… truncated to %d characters of context budget]"
	extraContextOmitted    = "\n\n[… %d further attached file(s) omitted for the %d character context budget: %s]"
	extraContextSeparator  = "\n\n"
	// maxExtraContextNameChars bounds a file NAME, not its content — a
	// reader's filename, not a budget-worthy document.
	maxExtraContextNameChars = 200
)

// extraContextFile is one attached file, already validated as a non-empty
// name paired with string content.
type extraContextFile struct {
	name    string
	content string
}

// extraContextSelection parses `extra_context`: a list of `{name, content}`
// objects. Anything else — a bare string, a number, an object missing either
// key — is refused rather than coerced, the same rule `contextSelection`
// applies to `context_paths`: a caller that sent the wrong shape gets told
// so, not silently ignored.
func extraContextSelection(value any) ([]extraContextFile, error) {
	if value == nil {
		return nil, nil
	}
	entries, ok := value.([]any)
	if !ok {
		return nil, spi.Failf(spi.KindValue, "%s must be a list of {name, content} files", ExtraContextParam)
	}
	files := make([]extraContextFile, 0, len(entries))
	for _, entry := range entries {
		obj, ok := entry.(map[string]any)
		if !ok {
			return nil, spi.Failf(spi.KindValue,
				"%s entries must be objects with name and content; refused %v", ExtraContextParam, entry)
		}
		name := strings.TrimSpace(str(obj["name"]))
		content, hasContent := obj["content"].(string)
		if name == "" || !hasContent {
			return nil, spi.Failf(spi.KindValue,
				"%s entries must carry a non-empty name and a string content", ExtraContextParam)
		}
		if len([]rune(name)) > maxExtraContextNameChars {
			name = string([]rune(name)[:maxExtraContextNameChars]) + "…"
		}
		files = append(files, extraContextFile{name: name, content: content})
	}
	return files, nil
}

// extraContextTruncate is `truncate` (contextpaths.go) with this file's own
// marker text — kept separate rather than shared so the two budgets can move
// independently without one file's constant silently bounding the other's.
func extraContextTruncate(body string, limit int) string {
	runes := []rune(body)
	if len(runes) <= limit {
		return body
	}
	return string(runes[:limit]) + fmt.Sprintf(extraContextTruncation, limit)
}

// BuildExtraContextBlock renders an already-validated file list under the two
// budgets — the same shape `BuildContextBlock` renders for wiki pages, minus
// the read: the content is already in hand, so there is nothing to fetch and
// nothing that can fail.
func BuildExtraContextBlock(files []extraContextFile) string {
	sections := make([]string, 0, len(files))
	var omitted []string
	spent := 0
	for _, file := range files {
		remaining := ExtraContextTotalBudgetChars - spent
		if remaining <= 0 {
			omitted = append(omitted, file.name)
			continue
		}
		body := extraContextTruncate(file.content, ExtraContextPerFileBudgetChars)
		if len([]rune(body)) > remaining {
			body = extraContextTruncate(body, remaining)
		}
		spent += len([]rune(body))
		sections = append(sections, "--- file: "+file.name+" ---\n"+body)
	}
	block := strings.Join(sections, extraContextSeparator)
	if len(omitted) > 0 {
		block += fmt.Sprintf(extraContextOmitted, len(omitted), ExtraContextTotalBudgetChars, strings.Join(omitted, ", "))
	}
	return block
}

// PrependExtraContext puts the attached-files block in front of `question`,
// with no "Current question:" trailer of its own — see the file header for
// why. A caller with no files gets `question` back unchanged.
func PrependExtraContext(question, block string) string {
	if block == "" {
		return question
	}
	return ExtraContextLeadIn + "\n" + block + extraContextSeparator + question
}

// consumeExtraContextParam is params without the one key this file spends.
func consumeExtraContextParam(params Params) Params {
	next := make(Params, len(params))
	for k, v := range params {
		if k == ExtraContextParam {
			continue
		}
		next[k] = v
	}
	return next
}

// ApplyExtraContext resolves `extra_context` into `params["question"]` and
// removes the key it spent, the same shape `ApplyContextPaths` has. A tool
// that takes no question is refused a non-empty selection rather than
// silently dropping it, for the same reason: an attachment sent to a tool
// that cannot honour it must not look like it was honoured.
func ApplyExtraContext(tool string, params Params) (Params, error) {
	files, err := extraContextSelection(params[ExtraContextParam])
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return consumeExtraContextParam(params), nil
	}
	if !ContextTools[tool] {
		return nil, spi.Failf(spi.KindValue,
			"%s is not supported by %s; attach files to ask or deep_research", ExtraContextParam, tool)
	}
	if len(files) > MaxExtraContextFiles {
		return nil, spi.Failf(spi.KindValue,
			"%s carries %d files; the limit is %d", ExtraContextParam, len(files), MaxExtraContextFiles)
	}

	block := BuildExtraContextBlock(files)
	next := consumeExtraContextParam(params)
	next["question"] = PrependExtraContext(str(params["question"]), block)
	return next, nil
}
