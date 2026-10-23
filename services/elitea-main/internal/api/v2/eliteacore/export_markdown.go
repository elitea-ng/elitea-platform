package eliteacore

// Markdown export — the `?format=md` branch of
// GET /elitea_core/export_import/prompt_lib/{projectID}/{entityID}.
//
// ## Why this exists
//
// The client has always asked for it. `shared/lib/download.ts`'s
// `buildMarkdownExportUrl` sets `format=md`, takes the response as a blob and
// saves it as `<name>.md` — a faithful port of the old app's
// `useExport.js:70-76`. This handler ignored the parameter and answered the
// JSON export to every request, so the product's "Export to Markdown" control
// downloaded a `.md` file containing an export DOCUMENT. Nothing failed: the
// download succeeded, the file had the right name, and only opening it showed
// JSON. Found by the R-M4 fixture-freshness gate, whose whole purpose is to
// force a periodic re-read of what the backend actually returns.
//
// ## What it is a port of
//
// legacy/plugins/elitea_core/api/v2/export_import.py:121-164 (the dispatch,
// the single-file vs ZIP rule and the response headers) over
// legacy/plugins/elitea_core/utils/export_import.py:
//   export_application_md            (:1103) — one file per version
//   _application_to_md               (:932)  — frontmatter + body
//   _extract_toolkits_for_md         (:646)  — agent toolkits
//   _extract_toolkits_for_md_pipeline(:706)  — pipeline toolkits
//   _filter_internal_keys            (:620)
//   _sanitize_pgvector_configuration (:522), now stricter: see below
//   create_zip_archive               (:1165)
// and slugify (export_import_utils.py:28).
//
// ## What it deliberately does NOT carry, and why
//
// Two of the legacy frontmatter keys have no producer on this side, so they
// are OMITTED rather than emitted empty — a key whose value is a guess is
// worse than an absent key, because the importer cannot tell them apart:
//
//   - `nested_agents` / `nested_pipelines`. `_extract_nested_agents_pipelines`
//     resolves application-type tools against every OTHER application in the
//     same export. This route exports ONE application (`WHERE id = $1`), so
//     there is no set to resolve against.
//   - `pipeline_settings` (the visual graph). `exportedVersions` does not
//     select it, so a pipeline's markdown carries its nodes but not their
//     on-screen positions.
//
// `skills` was a third. It is now written: `exportedSkills` builds the array
// `_extract_skills_for_md` reads, and `skillBlocks` below renders it (#611).
//
// Each is a gap in what this service EXPORTS, not in this file's rendering,
// and each is listed on the route's own doc comment so it is visible from the
// handler rather than only from here.
//
// ## One thing this does that the JSON export does not
//
// It sanitises. `_sanitize_pgvector_configuration` dropped
// `pgvector_configuration.connection_string` — a database credential — and
// `_filter_internal_keys` drops the project-local ids that cannot mean
// anything in the project a file is imported into. The `pgvector`
// configuration type is retired (application/configurations/retired.go), so
// this port drops the whole `pgvector_configuration` block that a toolkit
// saved before the retirement may still hold: a reference to a retired type
// is useless to an importer, and dropping it cannot leak its credential.

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// settingsExcludeKeys is SETTINGS_EXCLUDE_KEYS (export_import.py:519) — keys
// that identify a toolkit's configuration INSIDE the project it was exported
// from, and so can only mislead the project it is imported into.
var settingsExcludeKeys = map[string]bool{
	"configuration_uuid":       true,
	"configuration_project_id": true,
	"import_uuid":              true,
	"available_tools":          true,
	"selected_tools":           true,
}

var (
	// NOT `[^\w\s-]`. Go's RE2 `\w` is ASCII-only, while Python 3's `re` `\w`
	// on a str is Unicode-aware — so the literal transcription of
	// export_import_utils.py:28 stripped every non-Latin letter that the legacy
	// KEEPS. An agent named "日本語 Agent" slugged to "-agent" here and to
	// "日本語-agent" there, and a wholly non-Latin name slugged to "" and took
	// markdownFilename's fallback, so every such agent in a project downloaded
	// as the same "application.agent.md" and overwrote the last one.
	// `\p{L}\p{N}_` is what Python's `\w` actually means.
	slugStripPattern    = regexp.MustCompile(`[^\p{L}\p{N}_\s\p{Zs}-]`)
	slugSeparatorRegexp = regexp.MustCompile(`[-\s\p{Zs}]+`)
)

// slugify is export_import_utils.py:28, including its 50-CHARACTER truncation.
func slugify(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	text = slugStripPattern.ReplaceAllString(text, "")
	text = slugSeparatorRegexp.ReplaceAllString(text, "-")
	// Runes, not bytes: Python slices characters here, and now that non-ASCII
	// letters survive the strip, a byte slice could cut a UTF-8 rune in half
	// and put an invalid sequence in a filename.
	if runes := []rune(text); len(runes) > 50 {
		text = string(runes[:50])
	}
	return text
}

/* ── frontmatter ─────────────────────────────────────────────────────────── */

// orderedMap is an insertion-ordered YAML mapping.
//
// It exists because the legacy dumps with `sort_keys=False`, so the file's key
// order is the order `_application_to_md` writes them in — `name` first,
// `description` second, the model settings together — and a Go `map` would
// emit them alphabetically. Order is not cosmetic here: these files are read
// by people reviewing an agent, which is the entire reason the format exists.
type orderedMap struct {
	keys   []string
	values map[string]any
}

func newOrderedMap() *orderedMap {
	return &orderedMap{values: map[string]any{}}
}

// set appends a key. A nil or empty value is DROPPED, mirroring the legacy's
// `if x:` guards — every optional frontmatter key there is written only when
// truthy.
func (m *orderedMap) set(key string, value any) {
	if isEmptyValue(value) {
		return
	}
	if _, seen := m.values[key]; !seen {
		m.keys = append(m.keys, key)
	}
	m.values[key] = value
}

// setAlways appends a key whose value is written even when it is a zero — the
// the legacy runtime's `is not None` guards (temperature and top_p, where 0 is a real
// setting and dropping it would silently re-default the agent).
func (m *orderedMap) setAlways(key string, value any) {
	if value == nil {
		return
	}
	if _, seen := m.values[key]; !seen {
		m.keys = append(m.keys, key)
	}
	m.values[key] = value
}

func isEmptyValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	// The export document carries both slice shapes — `exportedVersionVariables`
	// returns `make([]map[string]any, 0)`, which is non-nil and empty. Without
	// this case it fell to `default: false` and every agent with no variables
	// got a `variables: []` key the legacy omits.
	case []map[string]any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	case float64:
		return typed == 0
	case bool:
		return !typed
	default:
		return false
	}
}

// MarshalYAML renders the map in insertion order.
func (m *orderedMap) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	for _, key := range m.keys {
		keyNode := &yaml.Node{}
		if err := keyNode.Encode(key); err != nil {
			return nil, fmt.Errorf("encode frontmatter key %q: %w", key, err)
		}
		valueNode := &yaml.Node{}
		if err := valueNode.Encode(m.values[key]); err != nil {
			return nil, fmt.Errorf("encode frontmatter value for %q: %w", key, err)
		}
		node.Content = append(node.Content, keyNode, valueNode)
	}
	return node, nil
}

/* ── value filtering ─────────────────────────────────────────────────────── */

// filterInternalKeys is `_filter_internal_keys` (:620): recursive, and it
// removes the key at ANY nesting depth rather than only at the top level.
func filterInternalKeys(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, inner := range typed {
			if settingsExcludeKeys[key] {
				continue
			}
			out[key] = filterInternalKeys(inner)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, inner := range typed {
			out = append(out, filterInternalKeys(inner))
		}
		return out
	default:
		return value
	}
}

// sanitizePgvector replaces `_sanitize_pgvector_configuration` (:522). The
// the legacy runtime kept the block's title and forced `private` false; the type is
// retired, so the whole block, and with it the connection string, is dropped.
func sanitizePgvector(settings map[string]any) map[string]any {
	out := make(map[string]any, len(settings))
	for key, value := range settings {
		if key != "pgvector_configuration" {
			out[key] = value
		}
	}
	return out
}

/* ── toolkits ────────────────────────────────────────────────────────────── */

// toolkitBlocks is `_extract_toolkits_for_md` (:646) and, when
// `includeApplications` is set, `_extract_toolkits_for_md_pipeline` (:706).
//
// The version's `tools` carry only an `import_uuid` reference; the full
// toolkit lives in the document's separate `toolkits` array, so this resolves
// one against the other. A tool whose uuid resolves to nothing yields a block
// with an empty name rather than being dropped — the same as the legacy, whose
// `toolkit_map.get(import_uuid, {})` defaults to an empty dict.
func toolkitBlocks(tools []any, toolkits []any, includeApplications bool) []any {
	byUUID := map[string]map[string]any{}
	for _, entry := range toolkits {
		toolkit, isMap := entry.(map[string]any)
		if !isMap {
			continue
		}
		if uuid, ok := toolkit["import_uuid"].(string); ok && uuid != "" {
			byUUID[uuid] = toolkit
		}
	}

	blocks := make([]any, 0, len(tools))
	for _, entry := range tools {
		tool, isMap := entry.(map[string]any)
		if !isMap {
			continue
		}
		uuid, _ := tool["import_uuid"].(string)
		if uuid == "" {
			continue
		}
		full := byUUID[uuid]
		toolType, _ := full["type"].(string)

		// An application-type tool is a nested agent. For an AGENT export the
		// legacy skips it (it would be exported as its own file); for a
		// PIPELINE it keeps a reference so the import can re-link it.
		if toolType == "application" {
			if !includeApplications {
				continue
			}
			block := newOrderedMap()
			block.set("toolkit", full["name"])
			block.set("type", toolType)
			block.set("import_uuid", uuid)
			blocks = append(blocks, block)
			continue
		}

		block := newOrderedMap()
		name, _ := full["name"].(string)
		block.setAlways("toolkit", name)
		block.set("type", toolType)
		block.set("meta", full["meta"])

		settings, hasSettings := full["settings"].(map[string]any)
		if hasSettings && len(settings) > 0 {
			filtered := filterInternalKeys(sanitizePgvector(settings))
			if asMap, ok := filtered.(map[string]any); ok && len(asMap) > 0 {
				block.set("settings", asMap)
			}
		}

		// `selected_tools` sits at the top level or inside settings, so the
		// the legacy runtime checks both before deciding there is no selection.
		selected := full["selected_tools"]
		if isEmptyValue(selected) && hasSettings {
			selected = settings["selected_tools"]
		}
		block.set("tools", selected)

		blocks = append(blocks, block)
	}
	return blocks
}

/* ── skills ──────────────────────────────────────────────────────────────── */

// skillBlocks is `_extract_skills_for_md` (:874).
//
// A markdown file has to stand on its own: the reader edits it and imports it
// again, and the import has nothing else to read. So each block carries enough
// to recreate the skill — its name, its description, the name of the attached
// version and that version's instructions — and not the reference the JSON
// document carries. The import wizard rebuilds both halves from these blocks
// (apps/elitea-ui .../importWizardParser.helpers.js, buildSkillsFromFrontmatter).
//
// A reference that resolves to no skill, or to no version of that skill, is
// SKIPPED and not written half-formed. The legacy makes the same choice and
// calls it the no-fallback principle: a block that names a version the file
// does not carry would import a skill with the wrong instructions, which is
// worse than a skill the reader can see is missing.
func skillBlocks(references []any, skills []any) []any {
	byUUID := map[string]map[string]any{}
	for _, entry := range skills {
		skill, isMap := entry.(map[string]any)
		if !isMap {
			continue
		}
		if uuid, ok := skill["import_uuid"].(string); ok && uuid != "" {
			byUUID[uuid] = skill
		}
	}

	blocks := make([]any, 0, len(references))
	for _, entry := range references {
		reference, isMap := entry.(map[string]any)
		if !isMap {
			continue
		}
		importUUID, _ := reference["import_uuid"].(string)
		skill := byUUID[importUUID]
		if skill == nil {
			continue
		}
		versionName, _ := reference["version_name"].(string)
		if versionName == "" {
			versionName = "base"
		}
		version := skillVersionNamed(skill["versions"], versionName)
		if version == nil {
			continue
		}
		block := newOrderedMap()
		block.setAlways("name", skill["name"])
		block.setAlways("description", skill["description"])
		block.setAlways("version", versionName)
		block.setAlways("instructions", version["instructions"])
		blocks = append(blocks, block)
	}
	return blocks
}

// skillVersionNamed finds one version of an exported skill by name.
func skillVersionNamed(versions any, name string) map[string]any {
	for _, entry := range toAnySlice(versions) {
		version, isMap := entry.(map[string]any)
		if !isMap {
			continue
		}
		if versionName, _ := version["name"].(string); versionName == name {
			return version
		}
	}
	return nil
}

/* ── one version → one markdown file ─────────────────────────────────────── */

// exportedFile is one entry of `export_application_md`'s `files` list.
type exportedFile struct {
	name    string
	content string
}

// markdownAgentType is `_application_to_md`'s own mapping (:957): the stored
// `openai` type is written as `agent`, and every other type is written as it
// stands (`react` stays `react`).
func markdownAgentType(raw string) string {
	if raw == "openai" {
		return "agent"
	}
	if raw == "" {
		return "react"
	}
	return raw
}

// markdownFilename is export_application_md:1163-1167. The `base` version is
// unqualified; every other version carries its own slug.
func markdownFilename(appName, versionName, rawAgentType string) string {
	kind := "agent"
	if rawAgentType == "pipeline" {
		kind = "pipeline"
	}
	appSlug := slugify(appName)
	if appSlug == "" {
		appSlug = "application"
	}
	if versionName == "" || versionName == "base" {
		return fmt.Sprintf("%s.%s.md", appSlug, kind)
	}
	versionSlug := slugify(versionName)
	if versionSlug == "" {
		versionSlug = "version"
	}
	return fmt.Sprintf("%s.%s.%s.md", appSlug, versionSlug, kind)
}

// applicationToMarkdown is `_application_to_md` (:932).
func applicationToMarkdown(app map[string]any, toolkits, skills []any, version map[string]any) (string, error) {
	versionName, _ := version["name"].(string)
	rawAgentType, _ := version["agent_type"].(string)
	agentType := markdownAgentType(rawAgentType)

	front := newOrderedMap()
	front.setAlways("name", app["name"])
	front.setAlways("description", app["description"])
	if versionName != "" && versionName != "base" {
		front.set("version", versionName)
	}

	if llm, ok := version["llm_settings"].(map[string]any); ok {
		front.set("model", llm["model_name"])
		// `is not None`, not `if`: a temperature or top_p of 0 is a real
		// setting, and dropping it would re-default the agent on import.
		front.setAlways("temperature", llm["temperature"])
		front.set("max_tokens", llm["max_tokens"])
		front.setAlways("top_p", llm["top_p"])
	}

	front.setAlways("agent_type", agentType)

	if meta, ok := version["meta"].(map[string]any); ok {
		front.set("step_limit", meta["step_limit"])
		front.set("internal_tools", meta["internal_tools"])
	}

	front.set("welcome_message", version["welcome_message"])
	front.set("conversation_starters", version["conversation_starters"])

	// toAnySlice, not a `.([]any)` assertion: `exportedVersions` stores this as
	// `[]map[string]any` (export_import.go:238), which no `[]any` assertion
	// satisfies. Asserting instead of normalising silently dropped the whole
	// `toolkits:` block from every exported file.
	tools := toAnySlice(version["tools"])

	body := ""
	if rawAgentType == "pipeline" {
		// A pipeline's instructions are YAML, and the legacy lifts them into
		// the frontmatter rather than leaving them in the body. Invalid YAML
		// is not an error: `except yaml.YAMLError: pass` leaves the keys off
		// and the file still describes the pipeline's toolkits and settings.
		if raw, ok := version["instructions"].(string); ok && raw != "" {
			var parsed map[string]any
			if err := yaml.Unmarshal([]byte(raw), &parsed); err == nil {
				front.set("state", filterInternalKeys(parsed["state"]))
				front.set("entry_point", parsed["entry_point"])
				front.set("interrupt_after", parsed["interrupt_after"])
				front.set("interrupt_before", parsed["interrupt_before"])
				front.set("nodes", filterInternalKeys(parsed["nodes"]))
			}
		}
		front.set("toolkits", toolkitBlocks(tools, toolkits, true))
		front.set("pipeline_settings", version["pipeline_settings"])
	} else {
		body, _ = version["instructions"].(string)
		front.set("toolkits", toolkitBlocks(tools, toolkits, false))
	}

	front.set("variables", version["variables"])
	front.set("skills", skillBlocks(toAnySlice(version["skills"]), skills))

	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	// PyYAML's block-style default is two spaces; yaml.v3 defaults to four.
	// The importer parses YAML rather than bytes, so this is for the human
	// reading a diff of two exports, one from each implementation.
	encoder.SetIndent(2)
	if err := encoder.Encode(front); err != nil {
		return "", fmt.Errorf("render frontmatter: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return "", fmt.Errorf("close frontmatter encoder: %w", err)
	}

	return fmt.Sprintf("---\n%s---\n\n%s", buffer.String(), body), nil
}

// markdownFilesFor is `export_application_md` (:1103): one file per version of
// every application in the document.
func markdownFilesFor(result map[string]any) ([]exportedFile, error) {
	applications, _ := result["applications"].([]map[string]any)
	toolkits := toAnySlice(result["toolkits"])
	skills := toAnySlice(result["skills"])

	files := make([]exportedFile, 0, len(applications))
	for _, app := range applications {
		appName, _ := app["name"].(string)
		versions := toAnySlice(app["versions"])
		for _, entry := range versions {
			version, isMap := entry.(map[string]any)
			if !isMap {
				continue
			}
			content, err := applicationToMarkdown(app, toolkits, skills, version)
			if err != nil {
				return nil, err
			}
			versionName, _ := version["name"].(string)
			rawAgentType, _ := version["agent_type"].(string)
			files = append(files, exportedFile{
				name:    markdownFilename(appName, versionName, rawAgentType),
				content: content,
			})
		}
	}
	return files, nil
}

// toAnySlice normalises the two shapes the export document uses for its lists
// — `[]map[string]any` as built by ExportImportGet, and `[]any` as it comes
// back out of anything that has been through JSON.
func toAnySlice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case []map[string]any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, item)
		}
		return out
	default:
		return nil
	}
}

/* ── the response ────────────────────────────────────────────────────────── */

// uniqueEntryName returns `name` the first time it is asked for it, and
// `<stem>-2.md`, `<stem>-3.md`, … for each repeat — so a collision costs a
// suffix rather than a file.
func uniqueEntryName(used map[string]int, name string) string {
	used[name]++
	if used[name] == 1 {
		return name
	}
	stem, suffix := name, ""
	if dot := strings.LastIndex(name, "."); dot > 0 {
		stem, suffix = name[:dot], name[dot:]
	}
	return fmt.Sprintf("%s-%d%s", stem, used[name], suffix)
}

// zipArchive is `create_zip_archive` (:1165).
func zipArchive(files []exportedFile) ([]byte, error) {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	used := make(map[string]int, len(files))
	for _, file := range files {
		// archive/zip accepts a duplicate name without complaint and every
		// extractor then overwrites, so two versions whose names slug alike
		// ("Release Candidate" and "release-candidate") would leave the archive
		// one file short with nothing reporting it. The export is a backup.
		name := uniqueEntryName(used, file.name)
		entry, err := archive.Create(name)
		if err != nil {
			return nil, fmt.Errorf("create zip entry %q: %w", name, err)
		}
		if _, err := entry.Write([]byte(file.content)); err != nil {
			return nil, fmt.Errorf("write zip entry %q: %w", name, err)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("close zip archive: %w", err)
	}
	return buffer.Bytes(), nil
}

// contentDispositionAttachment quotes the filename and, for a name with
// non-ASCII characters, adds the RFC 5987 `filename*` form beside it — the
// same two-form header the legacy runtime's own `content_disposition_attachment` writes.
func contentDispositionAttachment(filename string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, filename)
	if ascii == filename {
		return fmt.Sprintf(`attachment; filename="%s"`, ascii)
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, urlEncodePath(filename))
}

// urlEncodePath percent-encodes for the RFC 5987 `filename*` parameter, which
// takes percent-encoded UTF-8 rather than a quoted string.
func urlEncodePath(value string) string {
	var out strings.Builder
	for _, b := range []byte(value) {
		isUnreserved := (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') ||
			(b >= '0' && b <= '9') || b == '-' || b == '.' || b == '_' || b == '~'
		if isUnreserved {
			out.WriteByte(b)
			continue
		}
		fmt.Fprintf(&out, "%%%02X", b)
	}
	return out.String()
}

// writeMarkdownExport is export_import.py:124-164 — the `format=md` response.
//
// One file goes out as markdown; more than one goes out as a ZIP, because a
// single response cannot carry two documents and silently exporting only the
// first would lose every version but one.
func writeMarkdownExport(w http.ResponseWriter, entityID string, result map[string]any) {
	files, err := markdownFilesFor(result)
	if err != nil {
		// 400, matching the legacy's own `return {'errors': ...}, 400` for a
		// conversion failure — the export is readable, this rendering of it is
		// not, and that is a fault of the request's `format`.
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "render markdown export: " + err.Error()})
		return
	}
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "No applications to export"})
		return
	}

	w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")

	if len(files) == 1 {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", contentDispositionAttachment(files[0].name))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(files[0].content)) // connection committed; nothing to report
		return
	}

	archive, err := zipArchive(files)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": exportReadFailed})
		return
	}
	name := fmt.Sprintf("elitea_export_%s.zip", path.Base(entityID))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDispositionAttachment(name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(archive) // connection committed; nothing to report
}
