package toolkits

// The save-time credential gate, tested WITHOUT a database (#613).
//
// The sibling suite,
// internal/api/toolkit_credential_admission_postgres_integration_test.go, is
// the one that proves the gate is COMPOSED — it drives the real NewRouter and a
// real PostgreSQL. It also t.Skip()s wholesale without ELITEA_TEST_DATABASE_URL,
// which made it the only thing pinning two contracts that a plain `go test ./...`
// must not be able to break silently:
//
//   - the `loc` WIRE SHAPE. The web client keys each settings_errors entry by
//     loc[1] and drops anything shorter (toolkitForm.helpers.ts's locFieldKey),
//     so a one-element loc renders a failed save with no message anywhere.
//   - the TRIAGE. refuse / proceed / unavailable is where this gate's whole
//     value lives, and three of its branches (503, the all-non-refusable
//     fall-through, the caller-input refusal) had no coverage at all. The
//     caller-input branch was the hole: ErrInvalidCurrentToolkitSettings used
//     to PROCEED, which handed the request body an off switch — one extra key
//     named __elitea_frozen_configuration_v1, or 40 levels of dummy nesting,
//     turned a refusal into a 201 that persisted the foreign reference.
//
// Everything below runs everywhere, on stubs, through the real Create/Update
// handlers.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// ── fixtures ───────────────────────────────────────────────────────────────

// recordingToolkitRepo records the body each write RECEIVES, which is the only
// way to see what would have been persisted.
//
// Repository is embedded as a nil interface on purpose: every method this file
// does not implement panics loudly if the handler reaches it, so a test that
// silently starts exercising a different code path fails instead of passing.
type recordingToolkitRepo struct {
	Repository
	stored  map[string]any
	creates []map[string]any
	updates []map[string]any
}

func (r *recordingToolkitRepo) CreateToolkit(_ context.Context, _ string, body map[string]any) (map[string]any, error) {
	r.creates = append(r.creates, body)
	return map[string]any{"id": "11"}, nil
}

func (r *recordingToolkitRepo) UpdateToolkit(_ context.Context, _, _ string, body map[string]any) (map[string]any, error) {
	r.updates = append(r.updates, body)
	return map[string]any{"id": "11"}, nil
}

func (r *recordingToolkitRepo) GetToolkit(_ context.Context, _, _ string) (map[string]any, error) {
	if r.stored == nil {
		return map[string]any{"id": "11", "type": "github"}, nil
	}
	return r.stored, nil
}

func (r *recordingToolkitRepo) writes() int { return len(r.creates) + len(r.updates) }

// stubSettingsValidator stands in for
// *configurationapp.CurrentToolkitSettingsResolver.
type stubSettingsValidator struct {
	resolved map[string]any
	err      error
	calls    int
}

func (s *stubSettingsValidator) Resolve(
	_ context.Context,
	_ configurationapp.CurrentToolkitSettingsRequest,
) (map[string]any, error) {
	s.calls++
	return s.resolved, s.err
}

// newSettingsValidationRouter mounts the REAL Create and Update handlers behind
// an authenticated principal, because the gate reads auth.UserFromContext and
// skips entirely without one.
func newSettingsValidationRouter(repo Repository, validator ToolkitSettingsValidator) http.Handler {
	handler := NewHandlerWithRepo(repo, WithSettingsValidator(validator))
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A `user` principal with a positive numeric id: exactly what the
			// edge's forwarded identity produces, and what OwningUserID names.
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(),
				auth.User{ID: "7", UserID: "7", AuthType: "user"})))
		})
	})
	router.Post("/tools/prompt_lib/{projectID}", handler.Create)
	router.Put("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Update)
	return router
}

// githubSaveBody is the shape the web credential picker writes: a two-key
// reference and nothing else (credentialPicker.tsx's toStoredValue).
func githubSaveBody(extraSettings map[string]any) map[string]any {
	settings := map[string]any{
		"repository":           "octocat/hello-world",
		"github_configuration": map[string]any{"elitea_title": "ci-bot", "private": false},
	}
	for key, value := range extraSettings {
		settings[key] = value
	}
	return map[string]any{"name": "fixture", "type": "github", "settings": settings}
}

func serveSave(t *testing.T, router http.Handler, method, target string, body map[string]any) (int, string) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the request body: %v", err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

const (
	createTarget = "/tools/prompt_lib/1"
	updateTarget = "/tool/prompt_lib/1/11"
)

// ── DEFECT 1: caller-controlled invalidity must refuse ──────────────────────

// ErrInvalidCurrentToolkitSettings is raised only for input the REQUEST BODY
// controls: the reserved marker key, an over-long or control-char key or
// `type`, a tree past MaxCurrentToolkitSettingsDepth, and the node/string-byte
// budgets. Triaging it as "proceed" meant any of those turned the gate off for
// the rest of the same body — the credential reference beside them was then
// persisted unresolved, which is the entire condition this gate exists to stop.
func TestASaveTheResolverRefusesAsInvalidInputIsRefusedAndNotPersisted(t *testing.T) {
	t.Parallel()

	for _, save := range []struct {
		name   string
		method string
		target string
	}{
		{"create", http.MethodPost, createTarget},
		{"update", http.MethodPut, updateTarget},
	} {
		t.Run(save.name, func(t *testing.T) {
			t.Parallel()

			repo := &recordingToolkitRepo{}
			validator := &stubSettingsValidator{err: configurationapp.ErrInvalidCurrentToolkitSettings}
			router := newSettingsValidationRouter(repo, validator)

			status, body := serveSave(t, router, save.method, save.target, githubSaveBody(nil))

			if status != http.StatusBadRequest {
				t.Fatalf("a save the resolver refused as invalid input answered %d, want 400.\n"+
					"  2xx means ErrInvalidCurrentToolkitSettings is being triaged as 'proceed' again,\n"+
					"  which is a caller-controlled off switch for the whole gate. Body: %s", status, body)
			}
			if repo.writes() != 0 {
				t.Fatalf("the refused save still reached the repository with %#v", append(repo.creates, repo.updates...))
			}
			if validator.calls != 1 {
				t.Fatalf("the resolver was called %d times, want 1", validator.calls)
			}
			// No settings_errors: there is no violation, so no field to key one
			// to, and an entry the client cannot key is dropped silently there.
			// The page's generic banner is the only place this can be shown, and
			// it only appears when the body carries no keyable entries.
			if decodeSaveJSON(t, body)["settings_errors"] != nil {
				t.Fatalf("the refusal carries settings_errors it cannot key to a field; "+
					"the client drops those and shows nothing at all. Body: %s", body)
			}
		})
	}
}

// The marker refusal is UNCONDITIONAL — ahead of the resolver, and ahead of the
// nil-validator skip. A deployment with ELITEA_CONFIGURATIONS_ENABLED unset
// composes no validator and still writes to the column the claim-time
// materializer reads for a forged vault owner.
func TestAFrozenConfigurationMarkerInTheBodyIsRefusedAtAnyDepthAndWithoutAValidator(t *testing.T) {
	t.Parallel()

	nested := map[string]any{
		"padding": []any{
			map[string]any{"deeper": map[string]any{configurationapp.CurrentFrozenConfigurationMarker: true}},
		},
	}

	for _, probe := range []struct {
		name  string
		extra map[string]any
	}{
		{"at the top level of settings", map[string]any{configurationapp.CurrentFrozenConfigurationMarker: true}},
		{"inside the credential reference", map[string]any{
			"github_configuration": map[string]any{
				"elitea_title": "ci-bot", "private": false,
				configurationapp.CurrentFrozenConfigurationMarker: true,
			},
		}},
		{"three levels down, through an array", nested},
	} {
		for _, composed := range []struct {
			name      string
			validator ToolkitSettingsValidator
		}{
			{"with the resolver composed", &stubSettingsValidator{}},
			// The nil-validator case is the load-bearing one: it is the default
			// Helm chart's shape.
			{"with no resolver composed", nil},
		} {
			t.Run(probe.name+" "+composed.name, func(t *testing.T) {
				t.Parallel()

				repo := &recordingToolkitRepo{}
				router := newSettingsValidationRouter(repo, composed.validator)

				status, body := serveSave(t, router, http.MethodPost, createTarget, githubSaveBody(probe.extra))

				if status != http.StatusBadRequest {
					t.Fatalf("a save carrying %s answered %d, want 400.\n"+
						"  A 2xx persists the marker verbatim, which is what lets a user-controlled map\n"+
						"  forge a vault owner through configuration_project_id. Body: %s",
						configurationapp.CurrentFrozenConfigurationMarker, status, body)
				}
				if repo.writes() != 0 {
					t.Fatalf("the refused save still reached the repository with %#v", repo.creates)
				}
				message, _ := decodeSaveJSON(t, body)["error"].(string)
				if !bytes.Contains([]byte(message), []byte(configurationapp.CurrentFrozenConfigurationMarker)) {
					t.Fatalf("the refusal does not name the reserved key, so the caller cannot tell "+
						"which key to drop: %q", message)
				}
				if stub, ok := composed.validator.(*stubSettingsValidator); ok && stub.calls != 0 {
					t.Fatalf("the resolver ran %d times before the marker refusal; the marker check "+
						"must not depend on it", stub.calls)
				}
			})
		}
	}
}

// The negative control for the two refusals above: the byte-identical body
// WITHOUT the marker must save. Without this, a gate that refused everything
// would pass every assertion in this file.
func TestTheSameBodyWithoutTheMarkerStillSaves(t *testing.T) {
	t.Parallel()

	repo := &recordingToolkitRepo{}
	router := newSettingsValidationRouter(repo, &stubSettingsValidator{})

	if status, body := serveSave(t, router, http.MethodPost, createTarget, githubSaveBody(nil)); status != http.StatusCreated {
		t.Fatalf("a clean github save answered %d, want 201. Body: %s", status, body)
	}
	if len(repo.creates) != 1 {
		t.Fatalf("the accepted save reached the repository %d times, want 1", len(repo.creates))
	}
}

// ── DEFECT 3: the discard is the invariant ─────────────────────────────────

// refuseUnresolvableToolkitSettings uses ONLY the resolver's error and throws
// its returned map away. Reference mode returns the configuration EXPANDED and
// stamped with CurrentFrozenConfigurationMarker; copying that back into the body
// would replace a live reference with a frozen snapshot and forge a vault owner
// through configuration_project_id.
//
// Nothing pinned that. A reviewer turned the discard into a copy-back and the
// whole suite stayed green, while the created row became
// {"github_configuration":{"__elitea_frozen_configuration_v1":true,
// "configuration_project_id":1,…},"pgvector_configuration":{}} instead of the
// two-key reference the client sent. The Postgres suite could not see it either:
// its only stored-state check compares a row before and after a REFUSED update,
// so both sides move together.
//
// The stub therefore returns exactly what ReferenceMode would, and the
// assertion is byte-for-byte on what the repository RECEIVED.
func TestAnAcceptedSavePersistsTheRequestSettingsByteForByte(t *testing.T) {
	t.Parallel()

	// What the real resolver hands back for githubSaveBody: expanded, marked,
	// and carrying a vault owner that the request never named.
	resolved := map[string]any{
		"repository": "octocat/hello-world",
		"github_configuration": map[string]any{
			"base_url":                 "https://api.github.com",
			"configuration_project_id": 1,
			"configuration_uuid":       "cfg-1",
			"configuration_type":       "github",
			configurationapp.CurrentFrozenConfigurationMarker: true,
		},
		"pgvector_configuration": map[string]any{},
	}

	for _, save := range []struct {
		name    string
		method  string
		target  string
		want    int
		written func(*recordingToolkitRepo) []map[string]any
	}{
		{"create", http.MethodPost, createTarget, http.StatusCreated,
			func(r *recordingToolkitRepo) []map[string]any { return r.creates }},
		{"update", http.MethodPut, updateTarget, http.StatusOK,
			func(r *recordingToolkitRepo) []map[string]any { return r.updates }},
	} {
		t.Run(save.name, func(t *testing.T) {
			t.Parallel()

			repo := &recordingToolkitRepo{}
			router := newSettingsValidationRouter(repo, &stubSettingsValidator{resolved: resolved})

			request := githubSaveBody(nil)
			wantSettings := canonicalJSON(t, request["settings"])

			status, body := serveSave(t, router, save.method, save.target, request)
			if status != save.want {
				t.Fatalf("an accepted save answered %d, want %d. Body: %s", status, save.want, body)
			}
			written := save.written(repo)
			if len(written) != 1 {
				t.Fatalf("the repository received %d writes, want 1", len(written))
			}
			gotSettings := canonicalJSON(t, written[0]["settings"])
			if gotSettings != wantSettings {
				t.Fatalf("the persisted settings are not the ones the request sent.\n"+
					"  sent:      %s\n"+
					"  persisted: %s\n"+
					"  The resolver's OUTPUT must be discarded: it is the configuration expanded and\n"+
					"  stamped with %s, and persisting it forges a vault owner through\n"+
					"  configuration_project_id.",
					wantSettings, gotSettings, configurationapp.CurrentFrozenConfigurationMarker)
			}
		})
	}
}

// ── the wire shape, and the branches that had no coverage ──────────────────

// The `loc` contract, pinned where it always runs. loc[1] is the ONLY thing the
// web client keys an error by, and a one-element loc — which this handler's own
// /toolkit_validator route still emits — is dropped there without a trace.
func TestARefusedCredentialEmitsATwoElementLocPerViolation(t *testing.T) {
	t.Parallel()

	repo := &recordingToolkitRepo{}
	router := newSettingsValidationRouter(repo, &stubSettingsValidator{
		err: &configurationapp.CurrentToolkitSettingsValidationError{
			Violations: []configurationapp.CurrentToolkitSettingsViolation{
				{Field: "github_configuration", Code: configurationapp.CurrentToolkitConfigurationNotFoundCode},
				// Dropped: this gate does not refuse on it (see
				// refuseUnresolvableToolkitSettings).
				{Field: "api_key", Code: configurationapp.CurrentToolkitSecretNotSealedCode},
				// Dropped: a violation with no field cannot be keyed to one.
				{Field: "", Code: configurationapp.CurrentToolkitConfigurationForbiddenCode},
			},
		},
	})

	status, body := serveSave(t, router, http.MethodPost, createTarget, githubSaveBody(nil))
	if status != http.StatusBadRequest {
		t.Fatalf("a refused credential answered %d, want 400. Body: %s", status, body)
	}
	if repo.writes() != 0 {
		t.Fatalf("the refused save still reached the repository")
	}

	entries, ok := decodeSaveJSON(t, body)["settings_errors"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("want exactly 1 settings_errors entry (the refusable one), got %v. Body: %s",
			decodeSaveJSON(t, body)["settings_errors"], body)
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("settings_errors[0] is not an object. Body: %s", body)
	}
	loc, ok := entry["loc"].([]any)
	if !ok || len(loc) != 2 || loc[0] != "settings" || loc[1] != "github_configuration" {
		t.Fatalf("settings_errors[0].loc is %v, want [\"settings\" \"github_configuration\"].\n"+
			"  The web client keys each error by loc[1] and DISCARDS entries without one, so the\n"+
			"  user would see a failed save with no message anywhere.", entry["loc"])
	}
	if entry["code"] != string(configurationapp.CurrentToolkitConfigurationNotFoundCode) {
		t.Fatalf("settings_errors[0].code is %v, want %q",
			entry["code"], configurationapp.CurrentToolkitConfigurationNotFoundCode)
	}
	if message, _ := entry["msg"].(string); message == "" {
		t.Fatalf("settings_errors[0].msg is empty; the violation carries no text of its own, "+
			"so the handler must supply one. Body: %s", body)
	}
}

// The fall-through: every violation was one this gate does not refuse on, so
// the save proceeds. Refusing here would make an already-stored toolkit of one
// of the ten secret-bearing SDK types impossible to edit, including impossible
// to fix.
func TestASaveWhoseOnlyViolationsAreNonRefusableStillSucceeds(t *testing.T) {
	t.Parallel()

	repo := &recordingToolkitRepo{}
	router := newSettingsValidationRouter(repo, &stubSettingsValidator{
		err: &configurationapp.CurrentToolkitSettingsValidationError{
			Violations: []configurationapp.CurrentToolkitSettingsViolation{
				{Field: "api_key", Code: configurationapp.CurrentToolkitSecretNotSealedCode},
				{Field: "", Code: configurationapp.CurrentToolkitConfigurationNotFoundCode},
			},
		},
	})

	status, body := serveSave(t, router, http.MethodPost, createTarget, githubSaveBody(nil))
	if status != http.StatusCreated {
		t.Fatalf("a save whose only violations are non-refusable answered %d, want 201. Body: %s", status, body)
	}
	if len(repo.creates) != 1 {
		t.Fatalf("the accepted save reached the repository %d times, want 1", len(repo.creates))
	}
}

// The two SERVER-SIDE conditions that still proceed. Neither says anything
// about the credential the caller named and neither is anything the caller can
// influence or fix, so refusing would make every toolkit of the affected type
// unsavable while the pinned snapshot stays broken.
func TestSchemaConditionsTheCallerCannotInfluenceStillSave(t *testing.T) {
	t.Parallel()

	for _, condition := range []struct {
		name string
		err  error
	}{
		{"a type the pinned snapshot does not describe", configurationapp.ErrCurrentToolkitSchemaNotFound},
		{"a pinned schema that cannot be read", configurationapp.ErrCurrentToolkitSchemaInvalid},
	} {
		t.Run(condition.name, func(t *testing.T) {
			t.Parallel()

			repo := &recordingToolkitRepo{}
			router := newSettingsValidationRouter(repo, &stubSettingsValidator{err: condition.err})

			status, body := serveSave(t, router, http.MethodPost, createTarget, githubSaveBody(nil))
			if status != http.StatusCreated {
				t.Fatalf("%s answered %d, want 201. Body: %s", condition.name, status, body)
			}
			if len(repo.creates) != 1 {
				t.Fatalf("the accepted save reached the repository %d times, want 1", len(repo.creates))
			}
		})
	}
}

// The 503 branch. A briefly unreachable configuration store or vault is NOT a
// statement about the caller's input, and proceeding on it would silently
// reintroduce the hole this gate closes for as long as the outage lasts.
func TestAnUnavailableResolverAnswers503AndPersistsNothing(t *testing.T) {
	t.Parallel()

	for _, outage := range []struct {
		name string
		err  error
	}{
		{"a dependency failure", configurationapp.ErrCurrentToolkitSettingsDependency},
		{"a cancelled request", context.Canceled},
		{"an unrecognised error", errors.New("connection reset by peer")},
	} {
		t.Run(outage.name, func(t *testing.T) {
			t.Parallel()

			repo := &recordingToolkitRepo{}
			router := newSettingsValidationRouter(repo, &stubSettingsValidator{err: outage.err})

			status, body := serveSave(t, router, http.MethodPost, createTarget, githubSaveBody(nil))
			if status != http.StatusServiceUnavailable {
				t.Fatalf("%s answered %d, want 503.\n"+
					"  201 would mean an outage silently disables the gate; 400 would blame the\n"+
					"  user's input for the server's problem. Body: %s", outage.name, status, body)
			}
			if repo.writes() != 0 {
				t.Fatalf("the 503'd save still reached the repository with %#v", repo.creates)
			}
		})
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

func decodeSaveJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode the response body %q: %v", body, err)
	}
	return decoded
}

// canonicalJSON re-encodes through encoding/json, whose map keys are sorted, so
// a key ORDERING difference cannot fail (or pass) the comparison — only the
// content can.
func canonicalJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %#v: %v", value, err)
	}
	return string(encoded)
}
