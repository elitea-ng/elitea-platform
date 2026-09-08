package configurations

// The label a platform surface completes for itself.
//
// ## The failure this pins
//
// The create route refuses a body that does not carry every field the type's
// own schema requires (required_fields.go), and `label` is required by EVERY
// type the pinned registry carries. Neither platform panel has a label field:
// each collects ONE name, sends it as `elitea_title` and renders that same
// field in its listing. So every create from the admin LLM Proxy section —
// publishing a provider, adopting a platform model — answered 400 naming a
// field no control on the screen can fill.
//
// The completion runs in the same rewrite that forces `shared` and `section`,
// and it is asserted here rather than only through the browser because the
// rewrite is where the surface's own contract lives: what the panel does not
// offer, the surface supplies.
//
// The refusal itself is deliberately NOT relaxed. The project-scoped create
// still refuses a body naming neither a label nor a title, which is the case
// the API journey pins and the reference implementation refuses.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// rewrittenProviderBody runs one body through the provider surface's rewrite
// and returns what the delegated handler would then read.
func rewrittenProviderBody(t *testing.T, body string, creating bool) map[string]any {
	t.Helper()
	method := http.MethodPost
	if !creating {
		method = http.MethodPut
	}
	recorder := httptest.NewRecorder()
	rewritten, ok := providerHandler().rewriteGlobalProviderBody(
		recorder, providerRequest(method, "/", body), creating)
	if !ok {
		t.Fatalf("the rewrite refused %s: %d %s", body, recorder.Code, recorder.Body.String())
	}
	raw, err := io.ReadAll(rewritten.Body)
	if err != nil {
		t.Fatalf("read the rewritten body: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode the rewritten body %q: %v", string(raw), err)
	}
	return decoded
}

// TestACreateFromTheProviderPanelCarriesALabel is the regression.
//
// The body is the one the dialog sends: a name, a type, and the provider's own
// data fields. `label` is absent because the screen has no such control.
func TestACreateFromTheProviderPanelCarriesALabel(t *testing.T) {
	body := rewrittenProviderBody(t,
		`{"elitea_title":"autotest_platform_provider","type":"open_ai",`+
			`"data":{"api_base":"http://llm-mock:8090/v1"}}`, true)

	if body["label"] != "autotest_platform_provider" {
		t.Errorf("label = %#v, want the title the panel typed", body["label"])
	}
	// The completion must not disturb what the rewrite already forces.
	if body["section"] != GlobalProviderSection {
		t.Errorf("section = %#v, want %q", body["section"], GlobalProviderSection)
	}
	if body["shared"] != true {
		t.Errorf("shared = %#v, want true", body["shared"])
	}
}

// TestTheLabelCompletionHonoursWhatTheCallerSent — a caller that DOES send a
// label keeps it, and one that names the row with the legacy `name` alias gets
// the label the delegated handler will store the title under.
func TestTheLabelCompletionHonoursWhatTheCallerSent(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "an explicit label survives",
			body: `{"elitea_title":"autotest_p","label":"Shared OpenAI","type":"open_ai"}`,
			want: "Shared OpenAI",
		},
		{
			name: "the legacy name alias fills it",
			body: `{"name":"autotest_p_alias","type":"open_ai"}`,
			want: "autotest_p_alias",
		},
		{
			name: "a blank label is no label",
			body: `{"elitea_title":"autotest_p_blank","label":"   ","type":"open_ai"}`,
			want: "autotest_p_blank",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := rewrittenProviderBody(t, testCase.body, true)["label"]; got != testCase.want {
				t.Errorf("label = %#v, want %q", got, testCase.want)
			}
		})
	}
}

// TestAnUpdateInventsNoLabel.
//
// The delegated update is a PARTIAL write: it writes the fields the body
// carries and leaves the rest. Completing a label there would rewrite the
// stored one on every edit that changed something else — renaming a credential
// by editing its endpoint.
func TestAnUpdateInventsNoLabel(t *testing.T) {
	body := rewrittenProviderBody(t,
		`{"elitea_title":"autotest_p","data":{"api_base":"http://llm-mock:8090/v1"}}`, false)
	if _, present := body["label"]; present {
		t.Errorf("an update body grew a label: %#v", body["label"])
	}
}

// TestTheModelSurfaceCompletesItsLabelToo — the twin gap. The platform model
// dialog sends `{elitea_title, type, data}` for the same reason, and
// `llm_model` requires a label like every other type.
//
// The credential link is omitted so this test needs no database: an absent
// link is admitted by the rewrite (the delegated create then refuses it for the
// schema-required `data.ai_credentials`, which is that rule's business and is
// covered where it lives).
func TestTheModelSurfaceCompletesItsLabelToo(t *testing.T) {
	recorder := httptest.NewRecorder()
	rewritten, ok := providerHandler().rewriteGlobalModelBody(recorder,
		providerRequest(http.MethodPost, "/",
			`{"elitea_title":"autotest_platform_model","type":"llm_model",`+
				`"data":{"name":"autotest-model"}}`), true)
	if !ok {
		t.Fatalf("the model rewrite refused the dialog's body: %d %s",
			recorder.Code, recorder.Body.String())
	}
	raw, _ := io.ReadAll(rewritten.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode the rewritten body %q: %v", string(raw), err)
	}
	if body["label"] != "autotest_platform_model" {
		t.Errorf("label = %#v, want the title the dialog typed", body["label"])
	}
}
