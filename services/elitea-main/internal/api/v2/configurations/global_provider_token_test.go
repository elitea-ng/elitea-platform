package configurations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// The fields a global provider update changes, as the classifier reads them
// (ADR-0059 R1.2b, issue 2993).
func TestGlobalProviderChangesNameOnlyWhatChanged(t *testing.T) {
	stored := storedGlobalProvider{found: true, configType: "open_ai", data: map[string]any{
		"api_base": "http://a.invalid", "api_key": "{{secret.k}}", "description": "old",
	}}
	for _, tc := range []struct {
		name   string
		body   map[string]any
		want   []string
		action string
	}{
		{"label only", map[string]any{"label": "x", "section": "ai_credentials", "shared": true},
			[]string{"label"}, adminaccess.OutcomeSettings},
		{"the stored type restated", map[string]any{"type": "open_ai", "label": "x"},
			[]string{"label"}, adminaccess.OutcomeSettings},
		{"the stored data restated with a new description", map[string]any{"data": map[string]any{
			"api_base": "http://a.invalid", "api_key": "{{secret.k}}", "description": "new",
		}}, []string{"description"}, adminaccess.OutcomeSettings},
		{"a new endpoint", map[string]any{"data": map[string]any{"api_base": "http://b.invalid"}},
			[]string{"api_base"}, adminaccess.OutcomeRoute},
		{"a new type", map[string]any{"type": "azure_openai"}, []string{"type"}, adminaccess.OutcomeRoute},
		{"a data that is not an object", map[string]any{"data": "x"}, []string{"data"}, adminaccess.OutcomeRoute},
		{"a field this table does not know", map[string]any{"meta": map[string]any{}},
			[]string{"meta"}, adminaccess.OutcomeRoute},
	} {
		before := stored.dataCopy()
		mergeStoredGlobalProviderData(tc.body, stored)
		got := globalProviderChanges(tc.body, stored, before)
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: changes = %v, want %v", tc.name, got, tc.want)
		}
		if action := adminaccess.GlobalProviderUpdateAction(got); action != tc.action {
			t.Errorf("%s: action = %q, want %q", tc.name, action, tc.action)
		}
	}
	// A stored key the written data no longer holds (a switch to the keyless
	// mode drops the stored key after the merge) is a change too.
	got := globalProviderChanges(map[string]any{"data": map[string]any{"api_base": "http://a.invalid"}},
		stored, stored.dataCopy())
	slices.Sort(got)
	if !slices.Equal(got, []string{"api_key", "description"}) {
		t.Errorf("dropped keys: changes = %v", got)
	}
}

// Without a stored row every key the body names counts as changed.
func TestGlobalProviderChangesWithoutAStoredRowNameEveryKey(t *testing.T) {
	got := globalProviderChanges(map[string]any{"data": map[string]any{"description": "d"}}, storedGlobalProvider{}, nil)
	if !slices.Equal(got, []string{"description"}) {
		t.Errorf("changes = %v", got)
	}
}

// The rewrite refuses an inherit token the route outcome, and lets it make a
// settings change.
func TestTheRewriteAppliesTheTokenRule(t *testing.T) {
	token := func(body string) (*httptest.ResponseRecorder, bool) {
		recorder := httptest.NewRecorder()
		request := providerRequest(http.MethodPut, "/7", body)
		request = request.WithContext(adminaccess.ContextWithTokenCaller(request.Context(),
			adminaccess.TokenCaller{Access: adminaccess.TokenAccessInherit, StandingGlobalAdmin: true}))
		_, ok := providerHandler().rewriteGlobalProviderBody(recorder, request, false)
		return recorder, ok
	}
	if recorder, ok := token(`{"label":"renamed"}`); !ok {
		t.Fatalf("a label change was refused: %d %s", recorder.Code, recorder.Body.String())
	}
	recorder, ok := token(`{"data":{"api_base":"http://b.invalid"}}`)
	var refusal struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &refusal)
	if ok || recorder.Code != http.StatusForbidden || refusal.Code != adminaccess.CodeTokenCriticalAction {
		t.Fatalf("an endpoint change = %v %d %s, want the token refusal", ok, recorder.Code, recorder.Body.String())
	}
}

// guardTx answers the guard's locked read with one stored row.
type guardTx struct {
	pgx.Tx
	configType string
	data       string
}

type guardRow struct{ tx guardTx }

func (r guardRow) Scan(dest ...any) error {
	*dest[0].(*[]byte) = []byte(r.tx.data)
	*dest[1].(*string) = r.tx.configType
	return nil
}

func (tx guardTx) QueryRow(context.Context, string, ...any) pgx.Row { return guardRow{tx: tx} }

// The stored-row guard holds a token's settings update to the row it was
// classified against: a row that changed since is a 409, not a write back
// over the newer endpoint.
func TestTheStoredRowGuardRefusesARowThatChanged(t *testing.T) {
	read := map[string]any{"api_base": "http://a.invalid"}
	ctx := withStoredRowGuard(context.Background(), storedRowGuard{configType: "open_ai", data: read})
	if failure, err := checkStoredRowGuard(ctx,
		guardTx{configType: "open_ai", data: `{"api_base":"http://a.invalid"}`}, `"p_1"`, "7"); failure != nil || err != nil {
		t.Fatalf("the same row = %+v, %v", failure, err)
	}
	for _, tx := range []guardTx{
		{configType: "open_ai", data: `{"api_base":"http://moved.invalid"}`},
		{configType: "azure_openai", data: `{"api_base":"http://a.invalid"}`},
	} {
		failure, err := checkStoredRowGuard(ctx, tx, `"p_1"`, "7")
		if err != nil || failure == nil || failure.status != http.StatusConflict {
			t.Errorf("a changed row %+v = %+v, %v; want 409", tx, failure, err)
		}
	}
	if failure, err := checkStoredRowGuard(context.Background(), guardTx{}, `"p_1"`, "7"); failure != nil || err != nil {
		t.Fatalf("no guard = %+v, %v", failure, err)
	}
}
