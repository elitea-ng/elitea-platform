// dial_protocol_test.go — legacy issue #6707, the resolver and handler half.
//
// The protocol is stored on the llm_model row (`data.dial_protocol`) and must
// reach the account on the credential pin, together with the request-shape
// mark for the openai protocol. These tests read both off the context the
// ROUTER received, which is exactly what bifrost/core and the account read.
// The account half (account/dial_protocol_test.go) then proves what each
// protocol puts on the wire.
package llmproxy

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/policy"
)

// dialSpy records the pin and the request-type mark of the last chat dispatch.
type dialSpy struct {
	*dispatchSpy

	mu         sync.Mutex
	link       account.LinkedCredential
	pinned     bool
	changeType any
	kind       any
}

func (s *dialSpy) ChatCompletionRequest(
	ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest,
) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	s.mu.Lock()
	s.link, s.pinned = ctx.Value(account.ContextKeyLinkedCredential).(account.LinkedCredential)
	s.changeType = ctx.Value(schemas.BifrostContextKeyChangeRequestType)
	s.kind = ctx.Value(account.ContextKeyDispatchKind)
	s.mu.Unlock()
	return s.dispatchSpy.ChatCompletionRequest(ctx, req)
}

func (s *dialSpy) EmbeddingRequest(
	ctx *schemas.BifrostContext, req *schemas.BifrostEmbeddingRequest,
) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	s.mu.Lock()
	s.kind = ctx.Value(account.ContextKeyDispatchKind)
	s.mu.Unlock()
	return s.dispatchSpy.EmbeddingRequest(ctx, req)
}

func (s *dialSpy) dispatchKind() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kind
}

func (s *dialSpy) observed() (account.LinkedCredential, bool, any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.link, s.pinned, s.changeType
}

var _ LLMRouter = (*dialSpy)(nil)

// dialModelRow is a staging-shaped llm_model row with a dial_protocol value.
// rawProtocol is the JSON text of the field, or "" to omit it.
func dialModelRow(rawProtocol string) fakeModelRow {
	extra := ""
	if rawProtocol != "" {
		extra = `,"dial_protocol":` + rawProtocol
	}
	return fakeModelRow{
		title: "Team Model",
		data: []byte(fmt.Sprintf(
			`{"name":"dial-model","ai_credentials":{"elitea_title":"team-dial","private":false}%s}`, extra)),
	}
}

// dialExpandedModelRow is a model row in the EXPANDED link shape. Its
// configuration_type is what the row's author wrote, which need not be the
// credential row's own type.
func dialExpandedModelRow(linkType, rawProtocol string) fakeModelRow {
	return fakeModelRow{
		title: "Team Model",
		data: []byte(fmt.Sprintf(
			`{"name":"dial-model","dial_protocol":%s,"ai_credentials":{"elitea_title":"team-dial","private":false,`+
				`"configuration_type":%q,"configuration_uuid":"cred-dial","configuration_project_id":%q}}`,
			rawProtocol, linkType, mapProjectID)),
	}
}

func newDialHandler(t *testing.T, row fakeModelRow, credentialType string, opts ...HandlerOption) (http.Handler, *dialSpy) {
	t.Helper()
	spy := &dialSpy{dispatchSpy: newDispatchSpy()}
	db := &fakeModelDB{
		rows: []fakeModelRow{row},
		credsBySchema: map[string][]fakeCredentialRow{
			mapProjectID: {{id: "cred-dial", typ: credentialType, title: "team-dial"}},
		},
	}
	opts = append([]HandlerOption{WithModelResolver(NewModelResolver(ModelResolverConfig{DB: db}))}, opts...)
	h := NewHandler(spy, nil, nil, opts...)
	return h.route(), spy
}

func postDialChat(t *testing.T, h http.Handler) {
	t.Helper()
	rec := postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Team Model","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestDialProtocolReachesThePin walks the three protocols on an ai_dial model.
// The pin must carry the protocol, and only the openai protocol may mark the
// chat completion for conversion into a Responses request.
func TestDialProtocolReachesThePin(t *testing.T) {
	for _, tc := range []struct {
		name        string
		raw         string
		want        account.DialProtocol
		wantConvert bool
	}{
		{name: "absent is the default", raw: "", want: ""},
		{name: "azure is the default", raw: `"azure"`, want: ""},
		{name: "anthropic", raw: `"anthropic"`, want: account.DialProtocolAnthropic},
		{name: "openai", raw: `"openai"`, want: account.DialProtocolOpenAI, wantConvert: true},
		{name: "case and space are forgiven", raw: `" OpenAI "`, want: account.DialProtocolOpenAI, wantConvert: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, spy := newDialHandler(t, dialModelRow(tc.raw), account.DialCredentialType)
			postDialChat(t, h)

			if got, _ := spy.last(); got.provider != "azure" || got.model != "dial-model" {
				t.Fatalf("dispatched %s/%s, want azure/dial-model", got.provider, got.model)
			}
			link, pinned, changeType := spy.observed()
			if !pinned || link.ConfigID != "cred-dial" {
				t.Fatalf("pin = %+v (pinned=%v), want the ai_dial credential", link, pinned)
			}
			if link.DialProtocol != tc.want {
				t.Errorf("pin.DialProtocol = %q, want %q", link.DialProtocol, tc.want)
			}
			converts := changeType == schemas.ResponsesRequest
			if converts != tc.wantConvert {
				t.Errorf("change-request-type = %v, want conversion=%v", changeType, tc.wantConvert)
			}
		})
	}
}

// TestDialProtocolIgnoredForOtherCredentialTypes proves the field is a
// statement about an AI DIAL endpoint. The same row linked to an Azure OpenAI
// credential dispatches exactly as before the field existed.
func TestDialProtocolIgnoredForOtherCredentialTypes(t *testing.T) {
	for _, typ := range []string{"azure_open_ai", "open_ai_azure", "anthropic"} {
		t.Run(typ, func(t *testing.T) {
			h, spy := newDialHandler(t, dialModelRow(`"openai"`), typ)
			postDialChat(t, h)
			link, pinned, changeType := spy.observed()
			if !pinned {
				t.Fatal("the credential pin is missing; the link must still resolve")
			}
			if link.DialProtocol != "" || changeType != nil {
				t.Fatalf("pin.DialProtocol = %q, change-request-type = %v; want neither for a %s credential",
					link.DialProtocol, changeType, typ)
			}
		})
	}
}

// TestDialProtocolMalformedValueKeepsTheLink proves a value the gateway does
// not know falls back to the default protocol WITHOUT losing the credential
// link. A typed decode would have failed the whole row and dropped the
// provider with it.
func TestDialProtocolMalformedValueKeepsTheLink(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown":    `"bedrock"`,
		"number":     `7`,
		"object":     `{"x":1}`,
		"null":       `null`,
		"empty text": `""`,
	} {
		t.Run(name, func(t *testing.T) {
			h, spy := newDialHandler(t, dialModelRow(raw), account.DialCredentialType)
			postDialChat(t, h)
			if got, _ := spy.last(); got.provider != "azure" {
				t.Fatalf("provider = %q, want azure; the row lost its credential link", got.provider)
			}
			link, pinned, changeType := spy.observed()
			if !pinned || link.DialProtocol != "" || changeType != nil {
				t.Fatalf("pin = %+v (pinned=%v), change-request-type = %v; want the default protocol",
					link, pinned, changeType)
			}
		})
	}
}

// TestDialProtocolDoesNotFollowARoutingRewrite is the governance guard. A
// routing rule that sends the request to another provider clears the pin, and
// the openai protocol's conversion mark must not survive it: the chat request
// for the new target must stay a chat request.
func TestDialProtocolDoesNotFollowARoutingRewrite(t *testing.T) {
	snap := snapshotOf(t, govRow(policy.TypeRoutingRule, "away-from-dial", map[string]any{
		"cel":      `provider == "azure"`,
		"priority": 10.0,
		"targets": []any{
			map[string]any{"provider": "openai", "model": "gpt-4o", "weight": 1.0},
		},
	}))
	h, spy := newDialHandler(t, dialModelRow(`"openai"`), account.DialCredentialType,
		WithGovernancePolicy(fixedPolicy{snap}, nil, nil),
		WithRoutingPick(func(float64) float64 { return 0 }))
	postDialChat(t, h)

	if got, _ := spy.last(); got.provider != "openai" || got.model != "gpt-4o" {
		t.Fatalf("dispatched %s/%s, want the routed openai/gpt-4o", got.provider, got.model)
	}
	link, _, changeType := spy.observed()
	if link.DialProtocol != "" {
		t.Errorf("pin.DialProtocol = %q after a rewrite, want none", link.DialProtocol)
	}
	if changeType != nil {
		t.Errorf("change-request-type = %v after a rewrite; the routed chat request must stay chat", changeType)
	}
}

// TestDialProtocolFollowsTheCredentialRowType is the design guard. The
// expanded link shape lets a model row's author state the credential type. A
// row that calls an azure_open_ai credential "ai_dial" must not change the
// route or the auth header used against that credential; a row that misnames
// a real ai_dial credential still gets its protocol, because the credential
// row's own type decides.
func TestDialProtocolFollowsTheCredentialRowType(t *testing.T) {
	for _, tc := range []struct {
		name, linkType, rowType string
		want                    account.DialProtocol
	}{
		{name: "link claims ai_dial, row is azure_open_ai", linkType: "ai_dial", rowType: "azure_open_ai"},
		{name: "link claims ai_dial, row is open_ai_azure", linkType: "ai_dial", rowType: "open_ai_azure"},
		{name: "link and row agree", linkType: "ai_dial", rowType: "ai_dial", want: account.DialProtocolOpenAI},
		{name: "link misnames an ai_dial row", linkType: "azure_open_ai", rowType: "ai_dial", want: account.DialProtocolOpenAI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, spy := newDialHandler(t, dialExpandedModelRow(tc.linkType, `"openai"`), tc.rowType)
			postDialChat(t, h)
			link, pinned, changeType := spy.observed()
			if !pinned {
				t.Fatal("the credential pin is missing; the link must still resolve")
			}
			if link.DialProtocol != tc.want {
				t.Errorf("pin.DialProtocol = %q, want %q", link.DialProtocol, tc.want)
			}
			if converts := changeType == schemas.ResponsesRequest; converts != (tc.want == account.DialProtocolOpenAI) {
				t.Errorf("change-request-type = %v, want conversion only for a real ai_dial row", changeType)
			}
		})
	}
}

// TestDialProtocolNeedsAResolvedCredential: an expanded link whose title the
// resolver cannot find has no credential type it can trust, so the protocol
// does not apply.
func TestDialProtocolNeedsAResolvedCredential(t *testing.T) {
	row := dialExpandedModelRow("ai_dial", `"anthropic"`)
	row.data = []byte(strings.Replace(string(row.data), `"team-dial"`, `"not-in-scope"`, 1))
	h, spy := newDialHandler(t, row, account.DialCredentialType)
	postDialChat(t, h)
	link, _, _ := spy.observed()
	if link.DialProtocol != "" {
		t.Fatalf("pin.DialProtocol = %q for an unresolved credential, want none", link.DialProtocol)
	}
}

// TestDispatchKindReachesTheAccount: the account builds the DIAL deployment
// route per operation, so the chat and the embedding handlers must name their
// operation on the context core hands to the account.
func TestDispatchKindReachesTheAccount(t *testing.T) {
	h, spy := newDialHandler(t, dialModelRow(""), account.DialCredentialType)
	postDialChat(t, h)
	if got := spy.dispatchKind(); got != account.DispatchChat {
		t.Errorf("chat dispatch kind = %v, want %q", got, account.DispatchChat)
	}
	rec := postAs(t, h, "/llm/v1/embeddings", mapProjectID, `{"model":"Team Model","input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("embeddings status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := spy.dispatchKind(); got != account.DispatchEmbedding {
		t.Errorf("embedding dispatch kind = %v, want %q", got, account.DispatchEmbedding)
	}
}
