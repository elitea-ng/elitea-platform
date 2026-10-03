package pipelinetriggers_test

// GitLab webhooks for an inbound pipeline trigger (legacy issue 6664), through
// the HTTP route with a real row behind it. The shared harness and helpers are
// in pipelinetriggers_postgres_integration_test.go.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
)

// standardWebhookHeaders is what a GitLab webhook with a SIGNING TOKEN sends:
// the Standard Webhooks headers over `id.timestamp.body`, keyed by the
// base64 key behind the `whsec_` prefix.
func standardWebhookHeaders(t *testing.T, secret, id string, signedAt time.Time, body string) map[string]string {
	t.Helper()
	encoded, ok := strings.CutPrefix(secret, "whsec_")
	if !ok {
		t.Fatalf("secret %q is not in the whsec_ form a Standard Webhooks sender is given", secret)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode the signing key: %v", err)
	}
	timestamp := strconv.FormatInt(signedAt.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "." + body))
	return map[string]string{
		pipelinetriggers.StandardWebhooksIDHeader:        id,
		pipelinetriggers.StandardWebhooksTimestampHeader: timestamp,
		pipelinetriggers.StandardWebhooksSignatureHeader: "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)),
	}
}

// TestGitLabSecretTokenTriggerStartsARun is the call a GitLab webhook with a
// SECRET TOKEN makes: no Authorization header, the secret verbatim in
// X-Gitlab-Token, a push payload with no `input`, at the /gitlab url this
// service handed out.
func TestGitLabSecretTokenTriggerStartsARun(t *testing.T) {
	h := newHarness(t)
	_, tokenID, secret, url := h.mintSignedTrigger(
		t, homeProject, homeSchema, "GitLab push", ownerUserID, `{"type":"gitlab"}`)
	if !strings.HasSuffix(url, "/gitlab") {
		t.Fatalf("url = %q, want the /gitlab suffix", url)
	}
	stored := decode(t, h.do(t, http.MethodGet,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, mustVersion(t, h, tokenID)), "", nil))
	if stored["auth_mode"] != pipelinetriggers.AuthModeToken || stored["provider"] != pipelinetriggers.ProviderGitLab {
		t.Fatalf("stored mode = %v / %v, want token / gitlab", stored["auth_mode"], stored["provider"])
	}

	body := `{"object_kind":"push","ref":"refs/heads/main","commits":[]}`
	response := h.do(t, http.MethodPost, url, body,
		map[string]string{pipelinetriggers.GitLabTokenHeader: secret, "X-Gitlab-Event": "Push Hook"})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", response.Code, response.Body.String())
	}
	request, ok := h.start.last()
	if !ok || request.UserInput != "" || !request.AllowEmptyUserInput {
		t.Fatalf("dispatch = %+v (present=%v), want an empty, unattended start", request, ok)
	}

	// The carrier is a carrier, not a bypass: a wrong token in it is the one
	// refusal, and the plain url without the suffix still works for the same
	// row (the suffix is decoration).
	wrong := h.do(t, http.MethodPost, url, body, map[string]string{pipelinetriggers.GitLabTokenHeader: "not-it"})
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong X-Gitlab-Token: status = %d, want 401", wrong.Code)
	}
	plain := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), body,
		map[string]string{pipelinetriggers.GitLabTokenHeader: secret})
	if plain.Code != http.StatusAccepted {
		t.Fatalf("the bare url: status = %d, want 202; body = %s", plain.Code, plain.Body.String())
	}
}

// X-Gitlab-Token is accepted on an ordinary custom bearer trigger too: it is a
// carrier like X-Elitea-Trigger-Token, and the row's mode is what decides.
// The provider suffix, by contrast, must match the row.
func TestTheGitLabCarrierWorksOnACustomTriggerButTheSuffixMustMatch(t *testing.T) {
	h := newHarness(t)
	_, tokenID, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)
	response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), "",
		map[string]string{pipelinetriggers.GitLabTokenHeader: secret})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", response.Code, response.Body.String())
	}
	suffixed := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID)+"/gitlab", "",
		map[string]string{pipelinetriggers.GitLabTokenHeader: secret})
	if suffixed.Code != http.StatusUnauthorized {
		t.Fatalf("a /gitlab suffix on a custom trigger: status = %d, want 401", suffixed.Code)
	}
}

// TestGitLabSigningTokenTriggerStartsARun is the Standard Webhooks mode: the
// signature over `id.timestamp.body` admits the run, and every other shape is
// the one refusal.
func TestGitLabSigningTokenTriggerStartsARun(t *testing.T) {
	h := newHarness(t)
	_, _, secret, url := h.mintSignedTrigger(t, homeProject, homeSchema, "GitLab signed", ownerUserID,
		`{"type":"gitlab","auth_mode":"standard_webhooks_hmac"}`)
	if !strings.HasPrefix(secret, "whsec_") || !strings.HasSuffix(url, "/gitlab") {
		t.Fatalf("secret %q / url %q: want a whsec_ secret and a /gitlab url", secret, url)
	}

	body := `{"object_kind":"push","ref":"refs/heads/main"}`
	now := time.Now()
	response := h.do(t, http.MethodPost, url, body, standardWebhookHeaders(t, secret, "msg_1", now, body))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", response.Code, response.Body.String())
	}
	if h.start.count() != 1 {
		t.Fatalf("dispatches = %d, want 1", h.start.count())
	}

	for _, test := range []struct {
		name    string
		body    string
		headers map[string]string
	}{
		{"a replay outside the tolerance", body,
			standardWebhookHeaders(t, secret, "msg_2", now.Add(-pipelinetriggers.StandardWebhooksTolerance-time.Minute), body)},
		{"the right signature over a tampered body", body + " ",
			standardWebhookHeaders(t, secret, "msg_3", now, body)},
		{"no signature headers at all", body, nil},
		{"the secret as a bearer", body, map[string]string{"Authorization": "Bearer " + secret}},
		{"the secret in GitLab's token carrier", body, map[string]string{pipelinetriggers.GitLabTokenHeader: secret}},
	} {
		t.Run(test.name, func(t *testing.T) {
			refused := h.do(t, http.MethodPost, url, test.body, test.headers)
			if refused.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", refused.Code, refused.Body.String())
			}
		})
	}
	if h.start.count() != 1 {
		t.Fatalf("dispatches = %d, want 1 — no refusal may start a run", h.start.count())
	}
}

// The database refuses a mode or provider the Go side never wrote. 0139
// widened 0138's CHECK constraints; it did not remove them.
func TestTheWidenedConstraintsStillRefuseUnknownValues(t *testing.T) {
	h := newHarness(t)
	for _, statement := range []string{
		`UPDATE %s.pipeline_triggers SET auth_mode = 'something_else'`,
		`UPDATE %s.pipeline_triggers SET provider = 'bitbucket'`,
		`UPDATE %s.pipeline_triggers SET auth_mode = 'standard_webhooks_hmac', signature_header = ''`,
	} {
		_, _, _ = h.mintTrigger(t, homeProject, homeSchema, "Constraint probe", ownerUserID)
		if _, err := h.pool.Exec(context.Background(), fmt.Sprintf(statement, homeSchema)); err == nil {
			t.Fatalf("%q was accepted by the schema", statement)
		}
	}
}

// mustVersion finds the version a trigger belongs to.
func mustVersion(t *testing.T, h *harness, tokenID string) int64 {
	t.Helper()
	var versionID int64
	if err := h.pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT version_id FROM %s.pipeline_triggers WHERE token_id = $1`, homeSchema), tokenID).Scan(&versionID); err != nil {
		t.Fatalf("find the trigger's version: %v", err)
	}
	return versionID
}
