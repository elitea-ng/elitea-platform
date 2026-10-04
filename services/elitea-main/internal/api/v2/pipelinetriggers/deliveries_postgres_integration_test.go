package pipelinetriggers_test

// One run per signed delivery, and the review findings on PR #1027 that came
// with it: a replayed Standard Webhooks or GitHub delivery, the GitLab
// secret-token body cap, and a start-use-case refusal that is not the caller's
// input. The shared harness is in pipelinetriggers_postgres_integration_test.go.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

func (h *harness) deliveryRows(t *testing.T, tokenID string) int {
	t.Helper()
	var rows int
	if err := h.pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT count(*) FROM %s.pipeline_trigger_deliveries WHERE token_id = $1`, homeSchema), tokenID,
	).Scan(&rows); err != nil {
		t.Fatalf("count delivery rows: %v", err)
	}
	return rows
}

func (h *harness) triggerConversations(t *testing.T) int {
	t.Helper()
	var conversations int
	if err := h.pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT count(*) FROM %s.chat_conversations WHERE source = $1`, homeSchema),
		pipelinetriggers.TriggerConversationSource).Scan(&conversations); err != nil {
		t.Fatal(err)
	}
	return conversations
}

// The same signed GitLab delivery, sent twice INSIDE the tolerance window. The
// timestamp check passes both; the delivery log must stop the second run, and
// answer it with the first run's ids so a GitLab retry reads as a success.
func TestAStandardWebhooksReplayInsideTheWindowStartsNoSecondRun(t *testing.T) {
	h := newHarness(t)
	_, tokenID, secret, url := h.mintSignedTrigger(t, homeProject, homeSchema, "GitLab signed", ownerUserID,
		`{"type":"gitlab","auth_mode":"standard_webhooks_hmac"}`)

	body := `{"object_kind":"push","ref":"refs/heads/main"}`
	headers := standardWebhookHeaders(t, secret, "msg_replayed", time.Now(), body)

	first := h.do(t, http.MethodPost, url, body, headers)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first: status = %d, want 202; body = %s", first.Code, first.Body.String())
	}
	firstBody := decode(t, first)

	replay := h.do(t, http.MethodPost, url, body, headers)
	if replay.Code != http.StatusAccepted {
		t.Fatalf("replay: status = %d, want 202 (idempotent); body = %s", replay.Code, replay.Body.String())
	}
	replayBody := decode(t, replay)
	for _, field := range []string{"execution_id", "conversation_id", "events_url", "version_id"} {
		if replayBody[field] != firstBody[field] {
			t.Fatalf("replay %s = %v, want the first run's %v", field, replayBody[field], firstBody[field])
		}
	}
	if h.start.count() != 1 {
		t.Fatalf("dispatches = %d, want 1 — a replayed delivery must not start a second run", h.start.count())
	}
	if conversations := h.triggerConversations(t); conversations != 1 {
		t.Fatalf("trigger conversations = %d, want 1", conversations)
	}

	// A DIFFERENT delivery (new webhook-id, properly signed) is a new run.
	other := h.do(t, http.MethodPost, url, body, standardWebhookHeaders(t, secret, "msg_next", time.Now(), body))
	if other.Code != http.StatusAccepted || h.start.count() != 2 {
		t.Fatalf("a new delivery: status = %d, dispatches = %d; want 202 and 2", other.Code, h.start.count())
	}
	if rows := h.deliveryRows(t, tokenID); rows != 2 {
		t.Fatalf("delivery rows = %d, want 2", rows)
	}
}

// GitHub signs no timestamp, so before the delivery log a captured delivery
// started a run every time it was replayed, for the life of the secret. The
// key is the SIGNED body: X-GitHub-Delivery is not signed, so a replay that
// changes it is still the same delivery.
func TestAGitHubReplayStartsNoSecondRunEvenWithAFreshDeliveryHeader(t *testing.T) {
	h := newHarness(t)
	_, _, secret, url := h.mintSignedTrigger(
		t, homeProject, homeSchema, "Repository webhook", ownerUserID, `{"type":"github"}`)

	body := `{"ref":"refs/heads/main","after":"0a1b2c","repository":{"full_name":"acme/widgets"}}`
	signature := githubSignature(secret, body)
	first := h.do(t, http.MethodPost, url, body, map[string]string{
		pipelinetriggers.GitHubSignatureHeader: signature,
		"X-GitHub-Delivery":                    "72d3162e-cc78-11e3-81ab-4c9367dc0958",
	})
	if first.Code != http.StatusAccepted {
		t.Fatalf("first: status = %d, want 202; body = %s", first.Code, first.Body.String())
	}

	for _, deliveryID := range []string{
		"72d3162e-cc78-11e3-81ab-4c9367dc0958", // GitHub's own "Redeliver"
		"00000000-0000-4000-8000-000000000000", // an attacker's fresh GUID
		"",                                     // no delivery header at all
	} {
		headers := map[string]string{pipelinetriggers.GitHubSignatureHeader: signature}
		if deliveryID != "" {
			headers["X-GitHub-Delivery"] = deliveryID
		}
		replay := h.do(t, http.MethodPost, url, body, headers)
		if replay.Code != http.StatusAccepted {
			t.Fatalf("replay (%q): status = %d, want 202; body = %s", deliveryID, replay.Code, replay.Body.String())
		}
		if decode(t, replay)["execution_id"] != decode(t, first)["execution_id"] {
			t.Fatalf("replay (%q) was not answered with the first run", deliveryID)
		}
	}
	if h.start.count() != 1 {
		t.Fatalf("dispatches = %d, want 1", h.start.count())
	}

	// A different event is a different body, and runs.
	next := `{"ref":"refs/heads/main","after":"3d4e5f"}`
	second := h.do(t, http.MethodPost, url, next,
		map[string]string{pipelinetriggers.GitHubSignatureHeader: githubSignature(secret, next)})
	if second.Code != http.StatusAccepted || h.start.count() != 2 {
		t.Fatalf("a new event: status = %d, dispatches = %d; want 202 and 2", second.Code, h.start.count())
	}
}

// A refused delivery claims nothing, and a delivery whose ADMISSION failed
// releases its claim: the sender's retry must be a first try, not a replay.
func TestAFailedAdmissionDoesNotBurnTheDelivery(t *testing.T) {
	h := newHarness(t)
	_, tokenID, secret, url := h.mintSignedTrigger(
		t, homeProject, homeSchema, "Repository webhook", ownerUserID, `{"type":"github"}`)
	body := `{"ref":"refs/heads/main","after":"retry-me"}`
	headers := map[string]string{pipelinetriggers.GitHubSignatureHeader: githubSignature(secret, body)}

	wrong := h.do(t, http.MethodPost, url, body,
		map[string]string{pipelinetriggers.GitHubSignatureHeader: githubSignature("not-the-secret", body)})
	if wrong.Code != http.StatusUnauthorized || h.deliveryRows(t, tokenID) != 0 {
		t.Fatalf("a refused delivery: status = %d, rows = %d; want 401 and no claim",
			wrong.Code, h.deliveryRows(t, tokenID))
	}

	h.start.mu.Lock()
	h.start.err = errors.New("runtime outbox is down")
	h.start.mu.Unlock()
	failed := h.do(t, http.MethodPost, url, body, headers)
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed admission: status = %d, want 503; body = %s", failed.Code, failed.Body.String())
	}
	if rows := h.deliveryRows(t, tokenID); rows != 0 {
		t.Fatalf("a failed admission left %d claim(s) behind", rows)
	}

	h.start.mu.Lock()
	h.start.err = nil
	h.start.mu.Unlock()
	retried := h.do(t, http.MethodPost, url, body, headers)
	if retried.Code != http.StatusAccepted {
		t.Fatalf("the sender's retry: status = %d, want 202; body = %s", retried.Code, retried.Body.String())
	}
	var executionID string
	if err := h.pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT execution_id FROM %s.pipeline_trigger_deliveries WHERE token_id = $1`, homeSchema), tokenID,
	).Scan(&executionID); err != nil || executionID != "execution-1" {
		t.Fatalf("admitted delivery row: execution_id = %q, err = %v", executionID, err)
	}
}

// A copy that arrives while the first is still being admitted is told to
// retry, not admitted and not refused as a bad credential. A claim abandoned
// by a process that died mid-admission stops blocking after the in-flight
// timeout.
func TestACopyOfADeliveryInFlightIsToldToRetry(t *testing.T) {
	h := newHarness(t)
	_, tokenID, secret, url := h.mintSignedTrigger(t, homeProject, homeSchema, "GitLab signed", ownerUserID,
		`{"type":"gitlab","auth_mode":"standard_webhooks_hmac"}`)
	body := `{"object_kind":"merge_request"}`
	headers := standardWebhookHeaders(t, secret, "msg_in_flight", time.Now(), body)

	// Claim the delivery the way a concurrent first copy would: a row with
	// no run behind it yet.
	first := h.do(t, http.MethodPost, url, body, headers)
	if first.Code != http.StatusAccepted {
		t.Fatalf("seed: status = %d", first.Code)
	}
	if _, err := h.pool.Exec(context.Background(), fmt.Sprintf(
		`UPDATE %s.pipeline_trigger_deliveries SET execution_id = NULL, conversation_uuid = NULL WHERE token_id = $1`,
		homeSchema), tokenID); err != nil {
		t.Fatal(err)
	}

	copied := h.do(t, http.MethodPost, url, body, headers)
	if copied.Code != http.StatusServiceUnavailable || copied.Header().Get("Retry-After") == "" {
		t.Fatalf("in-flight copy: status = %d, Retry-After = %q; want 503 with Retry-After",
			copied.Code, copied.Header().Get("Retry-After"))
	}
	if h.start.count() != 1 {
		t.Fatalf("dispatches = %d, want 1", h.start.count())
	}

	if _, err := h.pool.Exec(context.Background(), fmt.Sprintf(
		`UPDATE %s.pipeline_trigger_deliveries SET received_at = now() - interval '10 minutes' WHERE token_id = $1`,
		homeSchema), tokenID); err != nil {
		t.Fatal(err)
	}
	reclaimed := h.do(t, http.MethodPost, url, body, headers)
	if reclaimed.Code != http.StatusAccepted || h.start.count() != 2 {
		t.Fatalf("an abandoned claim: status = %d, dispatches = %d; want 202 and 2",
			reclaimed.Code, h.start.count())
	}
}

// A bearer trigger keys nothing: its credential is the secret, not a signed
// delivery, and two identical bearer calls are two runs, as before.
func TestBearerCallsAreNotDeduplicated(t *testing.T) {
	h := newHarness(t)
	_, tokenID, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)
	for range 2 {
		response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), `{"input":"go"}`,
			map[string]string{"Authorization": "Bearer " + secret})
		if response.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", response.Code)
		}
	}
	if h.start.count() != 2 || h.deliveryRows(t, tokenID) != 0 {
		t.Fatalf("dispatches = %d, rows = %d; want 2 and 0", h.start.count(), h.deliveryRows(t, tokenID))
	}
}

// The GitLab SECRET-TOKEN preset is a bearer mode, but its body is GitLab's own
// event JSON. A push listing many commits passes 64 KiB as easily as a GitHub
// one, and was answered 413 under the bearer cap.
func TestALargeGitLabSecretTokenDeliveryStartsARun(t *testing.T) {
	h := newHarness(t)
	_, tokenID, secret, url := h.mintSignedTrigger(
		t, homeProject, homeSchema, "GitLab push", ownerUserID, `{"type":"gitlab"}`)

	commits := make([]string, 0, 20)
	for index := range 20 {
		commits = append(commits, fmt.Sprintf(
			`{"id":"%040d","message":"commit %d","added":[%s],"modified":[],"removed":[]}`,
			index, index, `"`+strings.Repeat("src/module/file.go", 1)+`",`+strings.Repeat(`"src/generated/very/long/path/name.ts",`, 280)+`"x"`))
	}
	body := `{"object_kind":"push","ref":"refs/heads/main","commits":[` + strings.Join(commits, ",") + `]}`
	if len(body) < 200*1024 {
		t.Fatalf("this case needs a ~200 KiB body; got %d bytes", len(body))
	}
	response := h.do(t, http.MethodPost, url, body,
		map[string]string{pipelinetriggers.GitLabTokenHeader: secret, "X-Gitlab-Event": "Push Hook"})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for a %d-byte GitLab delivery; body = %s",
			response.Code, len(body), response.Body.String())
	}
	if h.start.count() != 1 {
		t.Fatalf("dispatches = %d, want 1", h.start.count())
	}

	// The cap that remains is the read cap, for every trigger.
	huge := `{"object_kind":"push","pad":"` + strings.Repeat("A", 1100*1024) + `"}`
	refused := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), huge,
		map[string]string{pipelinetriggers.GitLabTokenHeader: secret})
	if refused.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over the read cap: status = %d, want 413", refused.Code)
	}
}

// The X-Gitlab-Token carrier when GitLab, or a proxy in front of it, also
// sends an Authorization header. Carrier precedence decides which value is
// compared, end to end through the route.
func TestTheGitLabCarrierBesideAnAuthorizationHeader(t *testing.T) {
	h := newHarness(t)
	_, _, secret, url := h.mintSignedTrigger(
		t, homeProject, homeSchema, "GitLab push", ownerUserID, `{"type":"gitlab"}`)
	for _, test := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"a wrong Bearer wins and is refused", map[string]string{
			"Authorization": "Bearer wrong", pipelinetriggers.GitLabTokenHeader: secret}, http.StatusUnauthorized},
		{"a Basic header is no carrier; the GitLab token is used", map[string]string{
			"Authorization": "Basic dXNlcjpwYXNz", pipelinetriggers.GitLabTokenHeader: secret}, http.StatusAccepted},
		{"a whitespace-only Elitea header falls through to the GitLab token", map[string]string{
			pipelinetriggers.TriggerTokenHeader: "   ", pipelinetriggers.GitLabTokenHeader: secret}, http.StatusAccepted},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := h.do(t, http.MethodPost, url, `{"object_kind":"push"}`, test.headers)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.want, response.Body.String())
			}
		})
	}
	if h.start.count() != 2 {
		t.Fatalf("dispatches = %d, want 2", h.start.count())
	}
}

// An ErrInvalidCurrentAgentStart that reaches the route has a cause the
// caller did not write: every input refusal is made before the use case. It
// is a 503 like any other start failure, not a 422 telling the sender to fix
// a body that was fine — and the empty conversation is still discarded.
func TestAStartRefusalThatIsNotTheInputIsNotBlamedOnTheInput(t *testing.T) {
	h := newHarness(t)
	_, tokenID, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)
	h.start.mu.Lock()
	h.start.err = fmt.Errorf("%w: frozen version snapshot refused", agentexecutionapp.ErrInvalidCurrentAgentStart)
	h.start.mu.Unlock()

	response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), `{"input":"fine"}`,
		map[string]string{"Authorization": "Bearer " + secret})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "`input`") {
		t.Fatalf("body = %s — a fault of ours must not name the caller's input", response.Body.String())
	}
	if conversations := h.triggerConversations(t); conversations != 0 {
		t.Fatalf("a refused start left %d trigger conversations behind", conversations)
	}
}
