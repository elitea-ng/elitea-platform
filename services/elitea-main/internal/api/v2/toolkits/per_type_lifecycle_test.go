package toolkits_test

// One lifecycle per SERVED toolkit type.
//
// The catalogue tests beside this file assert what the catalogue SAYS about a
// type. They do not create anything. Every write test in this package before
// this one used `github`, `custom` or a made-up type name, so the fifty-two SDK
// types the catalogue started serving were described by tests and exercised by
// none: a type whose settings schema the write path cannot carry would have
// been served as a tile, refused at save, and no test in this package would
// have moved.
//
// The table is the served catalogue itself, read out of the handler at run
// time, so a type added to the pinned SDK snapshot joins this suite with no
// edit here. The settings body of each case is DERIVED from that type's own
// served schema — see toolkitSettingsFixture — rather than hand-written, for
// the same reason: a hand-written table has to be extended by hand, and the way
// it fails when nobody extends it is by silently covering less.
//
// The fixture derivation is itself a gate. A schema shape the derivation cannot
// turn into a value fails the test naming the type and the property, which is
// what "every toolkit type has a fixture" has to mean if it is to fail when it
// stops being true.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

// lifecycleRepo is a Repository that remembers what the handler asked it to do.
//
// It answers from an in-memory map rather than returning the request body, so a
// read after a write proves the write path carried the settings rather than
// proving the mock echoes.
type lifecycleRepo struct {
	mockRepo

	rows      map[string]map[string]any
	nextID    int
	created   []map[string]any
	updated   []map[string]any
	deleted   []string
	forked    []map[string]any
	failWrite error
}

func newLifecycleRepo() *lifecycleRepo {
	return &lifecycleRepo{rows: map[string]map[string]any{}, nextID: 1}
}

func (r *lifecycleRepo) CreateToolkit(
	_ context.Context, _ string, body map[string]any,
) (map[string]any, error) {
	if r.failWrite != nil {
		return nil, r.failWrite
	}
	r.created = append(r.created, body)
	id := fmt.Sprintf("%d", r.nextID)
	r.nextID++
	row := map[string]any{"id": id}
	for key, value := range body {
		if strings.HasPrefix(key, "_") {
			continue
		}
		row[key] = value
	}
	r.rows[id] = row
	return row, nil
}

func (r *lifecycleRepo) GetToolkit(_ context.Context, _, toolkitID string) (map[string]any, error) {
	row, found := r.rows[toolkitID]
	if !found {
		return nil, fmt.Errorf("toolkit %q not found", toolkitID)
	}
	return row, nil
}

func (r *lifecycleRepo) UpdateToolkit(
	_ context.Context, _, toolkitID string, body map[string]any,
) (map[string]any, error) {
	if r.failWrite != nil {
		return nil, r.failWrite
	}
	r.updated = append(r.updated, body)
	row, found := r.rows[toolkitID]
	if !found {
		return nil, fmt.Errorf("toolkit %q not found", toolkitID)
	}
	for key, value := range body {
		row[key] = value
	}
	return row, nil
}

func (r *lifecycleRepo) DeleteToolkit(_ context.Context, _, toolkitID string) error {
	if _, found := r.rows[toolkitID]; !found {
		return fmt.Errorf("toolkit %q not found", toolkitID)
	}
	delete(r.rows, toolkitID)
	r.deleted = append(r.deleted, toolkitID)
	return nil
}

func (r *lifecycleRepo) ForkToolkit(
	_ context.Context, _ string, body map[string]any,
) (toolkits.Tool, error) {
	r.forked = append(r.forked, body)
	name, _ := body["name"].(string)
	forkedType, _ := body["type"].(string)
	settings, _ := body["settings"].(map[string]any)
	return toolkits.Tool{
		ID: fmt.Sprintf("fork-%d", len(r.forked)), Name: name,
		Type: forkedType, Settings: settings,
	}, nil
}

func (r *lifecycleRepo) ListToolkits(
	_ context.Context, _ string, _, _ int,
) ([]map[string]any, int, error) {
	rows := make([]map[string]any, 0, len(r.rows))
	for _, id := range sortedRowIDs(r.rows) {
		rows = append(rows, r.rows[id])
	}
	return rows, len(rows), nil
}

func sortedRowIDs(rows map[string]map[string]any) []string {
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// lifecycleRouter mounts every instance route this file drives, on the paths
// internal/api/router.go mounts them on.
func lifecycleRouter(repo toolkits.Repository, opts ...toolkits.Option) *chi.Mux {
	handler := toolkits.NewHandlerWithRepo(repo, opts...)
	router := chi.NewRouter()
	router.Get("/tools/prompt_lib/{projectID}", handler.List)
	router.Post("/tools/prompt_lib/{projectID}", handler.Create)
	router.Get("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Get)
	router.Put("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Update)
	router.Patch("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Update)
	router.Delete("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Delete)
	router.Post("/tools/fork/prompt_lib/{projectID}", handler.ForkToolkit)
	return router
}

func doJSON(
	t *testing.T, router *chi.Mux, method, path string, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

// ── the fixture derivation ─────────────────────────────────────────────────

// missingFixture names a schema node the derivation could not turn into a
// value. It carries the property path so the failure says which type and which
// field, not just that some type failed.
type missingFixture struct {
	property string
	reason   string
}

// toolkitSettingsFixture derives a settings body for one type from that type's
// SERVED schema.
//
// It walks `properties` and produces one value per property, exactly as the
// create form does: an enum takes its first member, a bounded integer takes a
// value inside its bounds, `selected_tools` takes the tools the type actually
// declares, and a `$ref` property — a credential picker — takes the reference
// shape the client posts.
//
// The second return value is every property it could NOT value. It is not an
// error return because the caller reports them all at once: one unhandled
// schema shape should name itself, not hide behind the first one.
func toolkitSettingsFixture(typeSchema map[string]any) (map[string]any, []missingFixture) {
	properties, _ := typeSchema["properties"].(map[string]any)
	settings := make(map[string]any, len(properties))
	var missing []missingFixture
	for _, name := range keysOf(properties) {
		property, ok := properties[name].(map[string]any)
		if !ok {
			missing = append(missing, missingFixture{name, "property is not an object"})
			continue
		}
		if name == "selected_tools" {
			settings[name] = selectedToolsFixture(property)
			continue
		}
		value, ok := schemaValueFixture(property)
		if !ok {
			missing = append(missing, missingFixture{
				name, fmt.Sprintf("no value for schema %s", compactSchema(property)),
			})
			continue
		}
		settings[name] = value
	}
	if len(properties) == 0 {
		missing = append(missing, missingFixture{
			"", "the served schema declares no properties at all",
		})
	}
	return settings, missing
}

// selectedToolsFixture picks the tools to enable.
//
// The served node carries the SDK's own array schema (items.enum) plus the
// args_schemas the handler joins onto it. Both are read: a type whose enum and
// argument schemas disagree would otherwise be covered by whichever this test
// happened to prefer.
func selectedToolsFixture(property map[string]any) []any {
	names := map[string]struct{}{}
	if args, ok := property["args_schemas"].(map[string]any); ok {
		for name := range args {
			names[name] = struct{}{}
		}
	}
	if items, ok := property["items"].(map[string]any); ok {
		if enum, ok := items["enum"].([]any); ok {
			for _, value := range enum {
				if name, ok := value.(string); ok {
					names[name] = struct{}{}
				}
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	// Two tools, not all of them: a settings body carrying sixty tool names
	// says nothing more than one carrying two, and the point of the fixture is
	// the round trip, not the size of it.
	selected := make([]any, 0, 2)
	for index, name := range ordered {
		if index >= 2 {
			break
		}
		selected = append(selected, name)
	}
	return selected
}

// schemaValueFixture produces one value for one property schema.
//
// ok=false means the shape is one this derivation does not understand. It is
// NOT a silent skip: the caller turns it into a named failure.
func schemaValueFixture(property map[string]any) (any, bool) {
	if _, referenced := property["$ref"]; referenced {
		// A credential picker. The client posts the saved-configuration
		// reference shape, and the write path carries it verbatim when no
		// settings validator is composed.
		return map[string]any{"id": 1, "name": "autotest-credential"}, true
	}
	if enum, ok := property["enum"].([]any); ok && len(enum) > 0 {
		return enum[0], true
	}
	if branches, ok := property["anyOf"].([]any); ok {
		for _, raw := range branches {
			branch, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if branchType, _ := branch["type"].(string); branchType == "null" {
				continue
			}
			if value, ok := schemaValueFixture(branch); ok {
				return value, true
			}
		}
		return nil, false
	}
	switch propertyType, _ := property["type"].(string); propertyType {
	case "string":
		if format, _ := property["format"].(string); format == "uri" {
			return "https://autotest.invalid/", true
		}
		return "autotest-value", true
	case "integer":
		return integerFixture(property), true
	case "number":
		return float64(integerFixture(property)), true
	case "boolean":
		return true, true
	case "array":
		return []any{}, true
	case "object":
		return map[string]any{}, true
	}
	// A property with neither a type, an enum, an anyOf nor a $ref. The
	// reference deployment has none; if the SDK grows one this must be
	// extended deliberately rather than defaulted to null.
	return nil, false
}

func integerFixture(property map[string]any) int {
	value := 1
	if minimum, ok := jsonNumber(property["minimum"]); ok && int(minimum) > value {
		value = int(minimum)
	}
	if exclusive, ok := jsonNumber(property["exclusiveMinimum"]); ok && int(exclusive)+1 > value {
		value = int(exclusive) + 1
	}
	return value
}

func jsonNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	}
	return 0, false
}

func compactSchema(property map[string]any) string {
	encoded, err := json.Marshal(property)
	if err != nil {
		return "<unencodable>"
	}
	if len(encoded) > 160 {
		return string(encoded[:160]) + "…"
	}
	return string(encoded)
}

// servedCatalogueFixtures reads the served catalogue and derives one fixture
// per type, failing on the first type it cannot value.
func servedCatalogueFixtures(t *testing.T) (map[string]any, map[string]map[string]any) {
	t.Helper()
	catalogue := getToolkitTypeCatalogue(t, catalogueOptions(t, "python")...)
	fixtures := make(map[string]map[string]any, len(catalogue))
	var report []string
	for _, toolkitType := range sortedKeys(catalogue) {
		typeSchema, ok := catalogue[toolkitType].(map[string]any)
		if !ok {
			report = append(report, toolkitType+": served entry is not an object")
			continue
		}
		settings, missing := toolkitSettingsFixture(typeSchema)
		for _, gap := range missing {
			report = append(report, fmt.Sprintf("%s.%s: %s", toolkitType, gap.property, gap.reason))
		}
		fixtures[toolkitType] = settings
	}
	if len(report) > 0 {
		t.Fatalf("%d served toolkit type(s) have no derivable settings fixture:\n  %s",
			len(report), strings.Join(report, "\n  "))
	}
	return catalogue, fixtures
}

// ── the tests ──────────────────────────────────────────────────────────────

// TestEveryServedToolkitTypeHasASettingsFixture is the gate the rest of this
// file depends on. It is separate from the lifecycle below so a schema the
// derivation cannot read reports as its own failure rather than as fifty-six
// lifecycle failures.
func TestEveryServedToolkitTypeHasASettingsFixture(t *testing.T) {
	t.Parallel()

	catalogue, fixtures := servedCatalogueFixtures(t)
	if len(fixtures) != len(catalogue) {
		t.Fatalf("derived %d fixtures for %d served types", len(fixtures), len(catalogue))
	}
	// The count is asserted so a catalogue that collapsed to the eight
	// hand-written types cannot pass this file by having every one of its
	// eight types covered.
	if len(catalogue) < 52 {
		t.Fatalf("the served catalogue holds %d types, want at least the 52 the pinned"+
			" SDK snapshot declares plus the elitea_core-native types: %v",
			len(catalogue), sortedKeys(catalogue))
	}
	for toolkitType, settings := range fixtures {
		if len(settings) == 0 {
			t.Errorf("%s: the derived settings fixture is empty", toolkitType)
		}
	}
}

// TestEveryServedToolkitTypeSurvivesTheInstanceLifecycle drives create → read →
// update → fork → delete for every served type.
//
// Each step is asserted on the STORED row rather than on the response body
// where it can be, because Create and Update both answer with something derived
// from the body they were handed and a handler that dropped `settings` on the
// floor would still answer with them.
func TestEveryServedToolkitTypeSurvivesTheInstanceLifecycle(t *testing.T) {
	t.Parallel()

	catalogue, fixtures := servedCatalogueFixtures(t)

	for _, toolkitType := range sortedKeys(catalogue) {
		settings := fixtures[toolkitType]
		t.Run(toolkitType, func(t *testing.T) {
			t.Parallel()

			repo := newLifecycleRepo()
			router := lifecycleRouter(repo)

			// ── create
			created := doJSON(t, router, http.MethodPost, "/tools/prompt_lib/1", map[string]any{
				"name":        "autotest-" + toolkitType,
				"description": "per-type lifecycle fixture",
				"type":        toolkitType,
				"settings":    settings,
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("create %s: status=%d body=%s", toolkitType, created.Code, created.Body.String())
			}
			var createdRow map[string]any
			if err := json.Unmarshal(created.Body.Bytes(), &createdRow); err != nil {
				t.Fatalf("decode create response: %v (%s)", err, created.Body.String())
			}
			id, _ := createdRow["id"].(string)
			if id == "" {
				t.Fatalf("create %s answered no id: %s", toolkitType, created.Body.String())
			}
			if len(repo.created) != 1 {
				t.Fatalf("create %s reached the repository %d times", toolkitType, len(repo.created))
			}
			if storedType, _ := repo.created[0]["type"].(string); storedType != toolkitType {
				t.Errorf("create %s stored type %q", toolkitType, storedType)
			}
			// The author is stamped by the handler, not by the caller: a body
			// that carried no user must not reach the repository without one.
			if author, _ := repo.created[0]["_author_id"].(string); author == "" {
				t.Errorf("create %s stored no _author_id", toolkitType)
			}
			assertSettingsRoundTrip(t, toolkitType, "create", settings, repo.created[0])

			// ── read
			read := doJSON(t, router, http.MethodGet, "/tool/prompt_lib/1/"+id, nil)
			if read.Code != http.StatusOK {
				t.Fatalf("read %s: status=%d body=%s", toolkitType, read.Code, read.Body.String())
			}
			var readRow map[string]any
			if err := json.Unmarshal(read.Body.Bytes(), &readRow); err != nil {
				t.Fatalf("decode read response: %v (%s)", err, read.Body.String())
			}
			if readType, _ := readRow["type"].(string); readType != toolkitType {
				t.Errorf("read %s answered type %q", toolkitType, readType)
			}
			assertSettingsRoundTrip(t, toolkitType, "read", settings, readRow)

			// ── update: the settings the form re-posts, with one field renamed
			// so the assertion cannot pass on the stored copy.
			updatedSettings := cloneSettings(settings)
			updatedSettings["selected_tools"] = []any{}
			update := doJSON(t, router, http.MethodPut, "/tool/prompt_lib/1/"+id, map[string]any{
				"name":     "autotest-" + toolkitType + "-renamed",
				"type":     toolkitType,
				"settings": updatedSettings,
			})
			if update.Code != http.StatusOK {
				t.Fatalf("update %s: status=%d body=%s", toolkitType, update.Code, update.Body.String())
			}
			if len(repo.updated) != 1 {
				t.Fatalf("update %s reached the repository %d times", toolkitType, len(repo.updated))
			}
			stored, err := repo.GetToolkit(t.Context(), "1", id)
			if err != nil {
				t.Fatalf("read back %s after update: %v", toolkitType, err)
			}
			if name, _ := stored["name"].(string); name != "autotest-"+toolkitType+"-renamed" {
				t.Errorf("update %s did not carry the new name; stored %q", toolkitType, name)
			}
			assertSettingsRoundTrip(t, toolkitType, "update", updatedSettings, stored)

			// ── fork
			fork := doJSON(t, router, http.MethodPost, "/tools/fork/prompt_lib/1", map[string]any{
				"name": "autotest-" + toolkitType + "-fork", "type": toolkitType,
				"settings": settings,
			})
			if fork.Code != http.StatusOK {
				t.Fatalf("fork %s: status=%d body=%s", toolkitType, fork.Code, fork.Body.String())
			}
			var forked map[string]any
			if err := json.Unmarshal(fork.Body.Bytes(), &forked); err != nil {
				t.Fatalf("decode fork response: %v (%s)", err, fork.Body.String())
			}
			if forkedType, _ := forked["type"].(string); forkedType != toolkitType {
				t.Errorf("fork %s answered type %q", toolkitType, forkedType)
			}
			assertSettingsRoundTrip(t, toolkitType, "fork", settings, forked)

			// ── delete
			deleted := doJSON(t, router, http.MethodDelete, "/tool/prompt_lib/1/"+id, nil)
			if deleted.Code != http.StatusNoContent {
				t.Fatalf("delete %s: status=%d body=%s", toolkitType, deleted.Code, deleted.Body.String())
			}
			if len(repo.deleted) != 1 || repo.deleted[0] != id {
				t.Errorf("delete %s asked the repository for %v", toolkitType, repo.deleted)
			}
			// The row is gone: a read after the delete must not answer 200.
			gone := doJSON(t, router, http.MethodGet, "/tool/prompt_lib/1/"+id, nil)
			if gone.Code != http.StatusNotFound {
				t.Errorf("read after delete %s: status=%d, want 404", toolkitType, gone.Code)
			}
		})
	}
}

// assertSettingsRoundTrip compares the settings the caller sent with the ones
// that came back, through JSON on both sides so an int/float64 difference in
// the decode does not read as a lost field.
func assertSettingsRoundTrip(
	t *testing.T, toolkitType, step string, sent map[string]any, carrier map[string]any,
) {
	t.Helper()
	got, ok := carrier["settings"].(map[string]any)
	if !ok {
		t.Errorf("%s %s: the carrier holds no settings object: %v", toolkitType, step, keysOf(carrier))
		return
	}
	for _, key := range keysOf(sent) {
		want, err := json.Marshal(sent[key])
		if err != nil {
			t.Fatalf("encode sent %s.%s: %v", toolkitType, key, err)
		}
		value, present := got[key]
		if !present {
			t.Errorf("%s %s: settings lost the %q field", toolkitType, step, key)
			continue
		}
		have, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encode received %s.%s: %v", toolkitType, key, err)
		}
		if string(want) != string(have) {
			t.Errorf("%s %s: settings.%s round-tripped as %s, sent %s",
				toolkitType, step, key, have, want)
		}
	}
}

func cloneSettings(settings map[string]any) map[string]any {
	clone := make(map[string]any, len(settings))
	for key, value := range settings {
		clone[key] = value
	}
	return clone
}

// TestEveryServedToolkitTypeIsRefusedWhenGuardrailsBlockIt pairs with the
// lifecycle above.
//
// The lifecycle proves a type can be created. This proves the refusal is not
// keyed on the eight hand-written names either: every served type, blocked, is
// refused on all three write surfaces. A refusal that only knew the old eight
// would let the other forty-eight past a deployment's guardrails, and the
// catalogue test next door cannot see that because it never writes.
func TestEveryServedToolkitTypeIsRefusedWhenGuardrailsBlockIt(t *testing.T) {
	t.Parallel()

	catalogue, fixtures := servedCatalogueFixtures(t)

	for _, toolkitType := range sortedKeys(catalogue) {
		settings := fixtures[toolkitType]
		t.Run(toolkitType, func(t *testing.T) {
			t.Parallel()

			repo := newLifecycleRepo()
			router := lifecycleRouter(repo, toolkits.WithGuardrails(
				&guardrailSourceStub{policy: guardrails.NewPolicy(
					guardrails.PolicyInput{BlockedToolkits: []string{toolkitType}},
				)},
			))
			body := map[string]any{
				"name": "autotest-blocked", "type": toolkitType, "settings": settings,
			}

			for _, surface := range []struct {
				name   string
				method string
				path   string
			}{
				{"create", http.MethodPost, "/tools/prompt_lib/1"},
				{"update", http.MethodPut, "/tool/prompt_lib/1/1"},
				{"fork", http.MethodPost, "/tools/fork/prompt_lib/1"},
			} {
				response := doJSON(t, router, surface.method, surface.path, body)
				if response.Code != http.StatusForbidden {
					t.Errorf("%s a blocked %s: status=%d, want 403: %s",
						surface.name, toolkitType, response.Code, response.Body.String())
				}
			}
			if len(repo.created)+len(repo.updated)+len(repo.forked) != 0 {
				t.Errorf("a blocked %s reached the repository: created=%d updated=%d forked=%d",
					toolkitType, len(repo.created), len(repo.updated), len(repo.forked))
			}
		})
	}
}
