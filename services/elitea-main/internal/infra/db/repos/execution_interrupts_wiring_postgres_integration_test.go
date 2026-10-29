package repos

// Wave 2a-2 wiring of the per-interrupt decision ledger against a real
// PostgreSQL (fanout-interrupt-decisions v1.1 §4.1, §7, §8, §9): card
// ingestion in the frame-accepting transaction, the private claim-fenced
// fetch/ACK routes over HTTP, the Main-emitted agent_hitl_resolved frame,
// park/wake settlement and the fanout_wake claim opt-in. Requires
// ELITEA_TEST_DATABASE_URL; every test skips without it.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
)

const interruptWorkloadIdentity = "spiffe://elitea.test/runtime/rust-worker"

// pendingFrame is an agent_interrupt_pending NodeEvent for the response's
// execution, carrying the card at response_metadata.interrupt_card_v1.
func (h *interruptHarness) pendingFrame(responseID string, card []byte, extra map[string]any) outputapp.NodeEventFrame {
	h.t.Helper()
	h.sequence++
	metadata := map[string]any{"interrupt_card_v1": json.RawMessage(card), "pause_progress": true}
	for key, value := range extra {
		metadata[key] = value
	}
	browser, err := json.Marshal(map[string]any{
		"type": "agent_interrupt_pending", "stream_id": "stream-1", "message_id": responseID, "execution_generation": "1",
		"sio_event": "chat_predict", "content": nil, "response_metadata": metadata,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return outputapp.NodeEventFrame{
		EventID:     fmt.Sprintf("frame-%s-%d", h.t.Name(), h.sequence),
		Fence:       runtimedomain.Fence{ExecutionID: h.executions[responseID], Generation: 1},
		BrowserData: browser,
	}
}

func (h *interruptHarness) ingest(frame outputapp.NodeEventFrame) error {
	return h.repo.shared.WithinTx(h.t.Context(), interruptWriteTx, func(tx sqlExecutor) error {
		return projectExecutionInterrupt(h.t.Context(), tx, 1, frame, "claim-frame")
	})
}

func (h *interruptHarness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(h.t.Context(), query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func TestFrameIngestRaisesTheCardInTheAcceptingTransaction(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key, card := h.card("fanout-interrupt-card-v1.json", nil)
	frame := h.pendingFrame(response, card, nil)
	if err := h.ingest(frame); err != nil {
		t.Fatal(err)
	}
	var frontier []byte
	var state string
	if err := h.pool.QueryRow(t.Context(), `SELECT state, frontier::text FROM elitea_runtime.execution_interrupts
WHERE root_response_id = $1::uuid AND interrupt_key = $2`, response, key).Scan(&state, &frontier); err != nil {
		t.Fatal(err)
	}
	// The private frontier is the member tier call id (a digest of the frozen
	// child thread), the fan-out node and the ordinal (v1.1 §4.1).
	if state != "PENDING" || !strings.Contains(string(frontier), `"child_thread": "fo1_fd47d5a520dc10ff429e8c38e13656305daf31781c78c477db942ad604db4031"`) ||
		!strings.Contains(string(frontier), `"fanout_node": "research"`) || !strings.Contains(string(frontier), `"ordinal": 1`) {
		t.Fatalf("raised row: state=%s frontier=%s", state, frontier)
	}
	// Idempotent on (root_response_id, interrupt_key) with byte equality.
	if err := h.ingest(frame); err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	_, changed := h.card("fanout-interrupt-card-v1.json", func(c map[string]any) {
		c["interrupt_key"] = key
		c["display"].(map[string]any)["message"] = "Different text under the same key"
	})
	if err := h.ingest(h.pendingFrame(response, changed, nil)); !errors.Is(err, outputapp.ErrNodeEventOutputConflict) {
		t.Fatalf("different bytes under the same key: %v", err)
	}
	if n := h.count(`SELECT count(*) FROM elitea_runtime.execution_interrupts WHERE root_response_id = $1::uuid`, response); n != 1 {
		t.Fatalf("rows after replays = %d", n)
	}
	if got := h.audit(response, key); len(got) != 1 || got[0] != "RAISED:1:claim=claim-frame" {
		t.Fatalf("audit = %v", got)
	}
}

func TestFrameIngestRefusesWhatIsNotAMemberCard(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	other := h.newResponse()
	_, coordinator := h.card("fanout-interrupt-card-v1.coordinator.json", nil)
	_, member := h.card("fanout-interrupt-card-v1.json", nil)
	_, twoMembers := h.card("fanout-interrupt-card-v1.json", func(c map[string]any) {
		path := c["parent_agent_path"].([]any)
		c["parent_agent_path"] = append(path, map[string]any{"call_id": "fo1_" + strings.Repeat("ab", 32), "name": "Second", "sibling_ordinal": 1})
	})
	wrongMessage := h.pendingFrame(response, member, nil)
	wrongMessage.BrowserData = bytes.Replace(wrongMessage.BrowserData, []byte(response), []byte(other), 1)
	for name, frame := range map[string]outputapp.NodeEventFrame{
		"coordinator card until root pauses move": h.pendingFrame(response, coordinator, nil),
		"legacy pause member on the same frame":   h.pendingFrame(response, member, map[string]any{"hitl_interrupts": []any{}}),
		"two member tiers":                        h.pendingFrame(response, twoMembers, nil),
		"another response's message id":           wrongMessage,
		"invalid card":                            h.pendingFrame(response, []byte(`{"schema":"elitea.pipeline.fanout-interrupt-card.v1"}`), nil),
	} {
		if err := h.ingest(frame); !errors.Is(err, outputapp.ErrInvalidNodeEventOutput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	other2 := h.pendingFrame(response, member, nil)
	other2.BrowserData = bytes.Replace(other2.BrowserData, []byte(`"agent_interrupt_pending"`), []byte(`"agent_tool_start"`), 1)
	if err := h.ingest(other2); err != nil {
		t.Fatalf("other frame types are not ledger input: %v", err)
	}
	if n := h.count(`SELECT count(*) FROM elitea_runtime.execution_interrupts`); n != 0 {
		t.Fatalf("a refused frame wrote %d cards", n)
	}
}

type resolvedFrameRow struct {
	executionID string
	frame       map[string]any
	raw         string
}

func (h *interruptHarness) resolvedFrames(responseID string) []resolvedFrameRow {
	h.t.Helper()
	rows, err := h.pool.Query(h.t.Context(), `SELECT execution_id, event_bytes FROM elitea_runtime.execution_replay_events
WHERE event_id LIKE 'interrupt-resolved:' || $1 || ':%' ORDER BY cursor`, responseID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []resolvedFrameRow
	for rows.Next() {
		var row resolvedFrameRow
		var raw []byte
		if err := rows.Scan(&row.executionID, &raw); err != nil {
			h.t.Fatal(err)
		}
		row.raw = string(raw)
		if err := json.Unmarshal(raw, &row.frame); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

func resolvedMetadata(t *testing.T, row resolvedFrameRow) map[string]any {
	t.Helper()
	metadata, _ := row.frame["response_metadata"].(map[string]any)
	resolved, _ := metadata["interrupt_resolved_v1"].(map[string]any)
	if row.frame["type"] != "agent_hitl_resolved" || resolved == nil || resolved["schema"] != domain.ResolvedSchema {
		t.Fatalf("not an agent_hitl_resolved frame: %s", row.raw)
	}
	return resolved
}

func TestDecideEmitsAResolvedFrameWithoutTheValue(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key := h.raise(response, "fanout-interrupt-card-v1.map-ask-user.json")
	const answer = "deploy to staging-cluster-7 first"
	if _, err := h.decide(interruptOwner, response, key, "resolved", "answer", answer); err != nil {
		t.Fatal(err)
	}
	if _, err := h.decide(interruptOwner, response, key, "resolved", "answer", answer); err != nil {
		t.Fatal(err)
	}
	frames := h.resolvedFrames(response)
	if len(frames) != 1 {
		t.Fatalf("decision + replay emitted %d frames", len(frames))
	}
	resolved := resolvedMetadata(t, frames[0])
	if resolved["state"] != "DECIDED" || resolved["action"] != "answer" || resolved["interrupt_key"] != key || resolved["revision"] != float64(2) {
		t.Fatalf("resolved = %v", resolved)
	}
	if strings.Contains(frames[0].raw, answer) || frames[0].executionID != h.executions[response] ||
		frames[0].frame["message_id"] != response || frames[0].frame["stream_id"] != "stream-1" {
		t.Fatalf("frame = %s", frames[0].raw)
	}
}

func TestStopAndRegenerateCloseCardsWithResolvedFrames(t *testing.T) {
	h := newInterruptHarness(t)
	stopped := h.newResponse()
	first := h.raise(stopped, "fanout-interrupt-card-v1.json")
	second := h.raise(stopped, "fanout-interrupt-card-v1.delegated-auth.json")
	if _, err := h.decide(interruptOwner, stopped, first, "stop", "approve", ""); err != nil {
		t.Fatal(err)
	}
	closed := closeOpen(t, h, stopped, domain.StateCancelled, domain.Closer{ActorUserID: interruptOwner})
	if len(closed) != 2 {
		t.Fatalf("closed = %+v", closed)
	}
	frames := h.resolvedFrames(stopped)
	if len(frames) != 3 { // DECIDED + two CANCELLED
		t.Fatalf("frames = %d", len(frames))
	}
	keys := map[string]bool{}
	for _, frame := range frames[1:] {
		resolved := resolvedMetadata(t, frame)
		if resolved["state"] != "CANCELLED" || resolved["action"] != nil {
			t.Fatalf("cancel frame = %v", resolved)
		}
		keys[resolved["interrupt_key"].(string)] = true
	}
	if !keys[first] || !keys[second] {
		t.Fatalf("cancelled keys = %v", keys)
	}
	regenerated := h.newResponse()
	key := h.raise(regenerated, "fanout-interrupt-card-v1.json")
	closeOpen(t, h, regenerated, domain.StateSuperseded, domain.Closer{ActorUserID: interruptAsker})
	frames = h.resolvedFrames(regenerated)
	if len(frames) != 1 || resolvedMetadata(t, frames[0])["state"] != "SUPERSEDED" || h.row(regenerated, key).state != "SUPERSEDED" {
		t.Fatalf("supersede frames = %+v", frames)
	}
}

// park runs the paused-finalize transaction: response row lock (the terminal
// projection's lock), then the settlement.
func (h *interruptHarness) park(responseID string, seen int64) (ParkSettlement, error) {
	var settlement ParkSettlement
	err := h.repo.projects.WithinProjectTx(h.t.Context(), 1, interruptWriteTx, func(tx sqlExecutor) error {
		var id int64
		if err := tx.QueryRow(h.t.Context(), `SELECT id FROM chat_message_group WHERE uuid = $1::uuid FOR UPDATE`, responseID).Scan(&id); err != nil {
			return err
		}
		var err error
		settlement, err = h.repo.SettleParkedResponse(h.t.Context(), tx, 1, responseID, h.executions[responseID], 1, seen)
		return err
	})
	return settlement, err
}

func (h *interruptHarness) wakes(responseID string) []string {
	h.t.Helper()
	rows, err := h.pool.Query(h.t.Context(), `SELECT state || ':' || decision_revision || ':' || idempotency_key
FROM elitea_runtime.execution_interrupt_wakes WHERE root_response_id = $1::uuid ORDER BY decision_revision`, responseID)
	if err != nil {
		h.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func TestParkSettlementAndDecisionAdmitExactlyOneWake(t *testing.T) {
	h := newInterruptHarness(t)

	// Park first, then decide: the decision admits the wake.
	parkedFirst := h.newResponse()
	a := h.raise(parkedFirst, "fanout-interrupt-card-v1.json")
	b := h.raise(parkedFirst, "fanout-interrupt-card-v1.map-ask-user.json")
	if s, err := h.park(parkedFirst, 0); err != nil || s.WakeQueued {
		t.Fatalf("park with nothing decided = %+v, %v", s, err)
	}
	if _, err := h.decide(interruptOwner, parkedFirst, a, "park-a", "approve", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.decide(interruptOwner, parkedFirst, b, "park-b", "answer", "yes"); err != nil {
		t.Fatal(err)
	}
	want := "QUEUED:1:continue-interrupt/" + parkedFirst + "/1"
	if got := h.wakes(parkedFirst); len(got) != 1 || got[0] != want {
		t.Fatalf("park then two decisions = %v, want [%s]", got, want)
	}

	// Decide first, then park having seen nothing: the park admits the wake.
	decidedFirst := h.newResponse()
	c := h.raise(decidedFirst, "fanout-interrupt-card-v1.json")
	if _, err := h.decide(interruptOwner, decidedFirst, c, "decide-c", "approve", ""); err != nil {
		t.Fatal(err)
	}
	if s, err := h.park(decidedFirst, 0); err != nil || !s.WakeQueued || s.DecisionRevision != 1 {
		t.Fatalf("park after a decision = %+v, %v", s, err)
	}

	// A park whose final fetch saw the decision, with the card already
	// consumed (applied before the park), records the park only.
	consumed := h.newResponse()
	fence := h.bindLiveClaim(consumed)
	d := h.raise(consumed, "fanout-interrupt-card-v1.json")
	h.raise(consumed, "fanout-interrupt-card-v1.map-ask-user.json")
	if _, err := h.decide(interruptOwner, consumed, d, "consume-d", "approve", ""); err != nil {
		t.Fatal(err)
	}
	fetch, err := h.repo.FetchDecided(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := "ckpt-park"
	if _, err := h.ack(fence, domain.Ack{InterruptKey: d, RequestID: fetch.Decisions[0].RequestID, Revision: 2,
		DecisionSHA256: fetch.Decisions[0].DecisionSHA256, Outcome: domain.AckApplied, ChildCheckpointID: &checkpoint}); err != nil {
		t.Fatal(err)
	}
	if s, err := h.park(consumed, 1); err != nil || s.WakeQueued {
		t.Fatalf("park that saw every decision = %+v, %v", s, err)
	}
	// A seen revision ahead of Main's is a ledger fault, never a wake.
	if _, err := h.park(consumed, 7); !errors.Is(err, domain.ErrLedgerFault) {
		t.Fatalf("seen ahead of the ledger: %v", err)
	}
	// Stop while parked: the park is cleared, no wake can start later.
	closeOpen(t, h, consumed, domain.StateCancelled, domain.Closer{ActorUserID: interruptOwner})
	if n := h.count(`SELECT count(*) FROM elitea_runtime.execution_interrupt_responses WHERE root_response_id = $1::uuid AND parked_execution_id IS NOT NULL`, consumed); n != 0 {
		t.Fatal("a stopped response stayed parked")
	}
	// Stop with a queued wake: the wake fails typed and admits nothing.
	closeOpen(t, h, parkedFirst, domain.StateCancelled, domain.Closer{ActorUserID: interruptOwner})
	if got := h.wakes(parkedFirst); len(got) != 1 || !strings.HasPrefix(got[0], "FAILED:1:") {
		t.Fatalf("queued wake after stop = %v", got)
	}
	// The partial unique index refuses a second active wake, whoever writes it.
	h.exec(`UPDATE elitea_runtime.execution_interrupt_wakes SET state = 'QUEUED', failure_code = NULL WHERE root_response_id = $1::uuid`, parkedFirst)
	_, err = h.pool.Exec(t.Context(), `INSERT INTO elitea_runtime.execution_interrupt_wakes
(root_response_id, decision_revision, project_id, parked_execution_id, parked_generation, idempotency_key, state)
VALUES ($1::uuid, 2, 1, $2, 1, 'continue-interrupt/' || $1 || '/2', 'QUEUED')`, parkedFirst, h.executions[parkedFirst])
	if err == nil || !strings.Contains(err.Error(), "execution_interrupt_wakes_one_active") {
		t.Fatalf("second active wake: %v", err)
	}
	_, err = h.pool.Exec(t.Context(), `INSERT INTO elitea_runtime.execution_interrupt_wakes
(root_response_id, decision_revision, project_id, parked_execution_id, parked_generation, idempotency_key, state)
VALUES ($1::uuid, 3, 1, $2, 1, 'continue-interrupt/chosen-key', 'DONE')`, parkedFirst, h.executions[parkedFirst])
	if err == nil {
		t.Fatal("a wake key that is not derived from (response, revision) was stored")
	}
}

// TestDecideVersusParkExactlyOneContinuation races the decision transaction
// against the paused-finalize transaction. Each round holds the response row
// lock in a third transaction, queues both behind it in a chosen order (park
// first in even rounds, decision first in odd rounds), then releases it, so
// both interleavings run under real lock contention. Whichever commits second
// admits the wake; exactly one wake row exists every round.
func TestDecideVersusParkExactlyOneContinuation(t *testing.T) {
	h := newInterruptHarness(t)
	const rounds = 20
	parkedWakes, decidedWakes := 0, 0
	for i := range rounds {
		response := h.newResponse()
		key := h.raise(response, "fanout-interrupt-card-v1.json")
		holder, err := h.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := holder.Exec(t.Context(), `SELECT 1 FROM p_1.chat_message_group WHERE uuid = $1::uuid FOR UPDATE`, response); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var parkErr, decideErr error
		var settlement ParkSettlement
		park := func() { settlement, parkErr = h.park(response, 0) }
		decide := func() {
			_, decideErr = h.decide(interruptOwner, response, key, fmt.Sprintf("race-%d", i), "approve", "")
		}
		first, second := park, decide
		if i%2 == 1 {
			first, second = decide, park
		}
		wg.Go(first)
		h.waitForLockWaiters(1)
		wg.Go(second)
		h.waitForLockWaiters(2)
		if err := holder.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if parkErr != nil || decideErr != nil {
			t.Fatalf("round %d: park=%v decide=%v", i, parkErr, decideErr)
		}
		got := h.wakes(response)
		if len(got) != 1 || got[0] != "QUEUED:1:continue-interrupt/"+response+"/1" {
			t.Fatalf("round %d: wakes = %v", i, got)
		}
		if settlement.WakeQueued {
			parkedWakes++
		} else {
			decidedWakes++
		}
		if n := h.count(`SELECT count(*) FROM elitea_runtime.execution_interrupt_responses WHERE root_response_id = $1::uuid AND parked_execution_id IS NOT NULL`, response); n != 0 {
			t.Fatalf("round %d: response still parked with a wake queued", i)
		}
	}
	// Both orders happened: park-then-decide (the decision admits the wake)
	// and decide-then-park (the settlement admits it).
	if parkedWakes != rounds/2 || decidedWakes != rounds/2 {
		t.Fatalf("wake admitted by park=%d decision=%d, want %d each", parkedWakes, decidedWakes, rounds/2)
	}
}

func (h *interruptHarness) claimTLS() *tls.ConnectionState {
	identity, _ := url.Parse(interruptWorkloadIdentity)
	return verifiedPeer(&x509.Certificate{URIs: []*url.URL{identity}})
}

// verifiedPeer is the TLS state of a caller whose certificate verified at the handshake.
func verifiedPeer(certificate *x509.Certificate) *tls.ConnectionState {
	return &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{certificate}}, PeerCertificates: []*x509.Certificate{certificate}}
}

type noContentAuthorizer struct{}

func (noContentAuthorizer) AuthorizeContent(context.Context, storage.ContentClaim) (storage.ContentAuthorization, error) {
	return storage.ContentAuthorization{}, storage.ErrContentUnauthorized
}

type noContentStore struct{}

func (noContentStore) OpenContent(context.Context, string, string, string, string) (io.ReadCloser, error) {
	return nil, storage.ErrContentNotFound
}

type privateCall struct {
	claimID     string
	fence       []byte
	execution   string
	tlsState    *tls.ConnectionState
	body        []byte
	userSession bool
}

func (h *interruptHarness) privateRoutes() *storage.ContentServer {
	h.t.Helper()
	server, err := storage.NewContentServer(noContentAuthorizer{}, noContentStore{}, 8192)
	if err != nil {
		h.t.Fatal(err)
	}
	return server.WithExecutionInterrupts(h.repo)
}

// privateAnswer is one RPC's outcome as the HTTP status of the same contract
// row (v1.1 §7.1) and the canonical body.
type privateAnswer struct {
	Code int
	Body *bytes.Buffer
}

// callPrivate calls the private fetch or ACK RPC as the Worker does: a
// verified client certificate (tlsState) and the claim metadata.
func callPrivate(server *storage.ContentServer, rpc string, call privateCall) privateAnswer {
	ctx := context.Background()
	if call.tlsState != nil {
		ctx = peer.NewContext(ctx, &peer.Peer{AuthInfo: credentials.TLSInfo{State: *call.tlsState}})
	}
	var pairs []string
	if call.claimID != "" {
		pairs = append(pairs, grpcauth.MetadataClaimID, call.claimID)
	}
	if call.fence != nil {
		pairs = append(pairs, grpcauth.MetadataClaimFence, string(call.fence))
	}
	if call.userSession {
		// A browser identity is never authority on the private plane.
		pairs = append(pairs, "cookie", "elitea_session=opaque", "authorization", "Bearer browser")
	}
	if len(pairs) > 0 {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(pairs...))
	}
	scope := &runtimev1.RuntimeContentScopeV1{ExecutionId: call.execution, Generation: 1}
	var body []byte
	var err error
	if rpc == "fetch" {
		response, callErr := server.FetchExecutionInterrupts(ctx, &runtimev1.FetchExecutionInterruptsRequestV1{Scope: scope})
		body, err = response.GetFetchJson(), callErr
	} else {
		response, callErr := server.AcknowledgeExecutionInterrupt(ctx, &runtimev1.AcknowledgeExecutionInterruptRequestV1{Scope: scope, AckJson: call.body})
		body, err = response.GetOutcomeJson(), callErr
	}
	status := map[codes.Code]int{
		codes.OK: http.StatusOK, codes.PermissionDenied: http.StatusForbidden, codes.InvalidArgument: http.StatusBadRequest,
		codes.NotFound: http.StatusNotFound, codes.AlreadyExists: http.StatusConflict, codes.Unavailable: http.StatusServiceUnavailable,
	}[grpcstatus.Code(err)]
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return privateAnswer{Code: status, Body: bytes.NewBuffer(body)}
}

// TestPrivateDecisionRoutesOverGRPC covers the private fetch/ACK RPCs end to
// end (contract v1.1 §7.1), including the claim matrix: the live claim, an
// unauthenticated caller (no client certificate), a user session, a foreign
// workload identity, another execution's claim, a stale fence, a
// NODE_RECOVERY claim, and the old claim after a takeover.
func TestPrivateDecisionRoutesOverGRPC(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	fence := h.bindLiveClaim(response)
	otherResponse := h.newResponse()
	otherFence := h.bindLiveClaim(otherResponse)
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	foreignKey := h.raise(otherResponse, "fanout-interrupt-card-v1.json")
	if _, err := h.decide(interruptOwner, response, key, "http", "approve", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.decide(interruptOwner, otherResponse, foreignKey, "http-other", "approve", ""); err != nil {
		t.Fatal(err)
	}
	routes := h.privateRoutes()
	live := privateCall{claimID: fence.ClaimID, fence: fence.FenceToken, execution: fence.ExecutionID, tlsState: h.claimTLS()}

	w := callPrivate(routes, "fetch", live)
	if w.Code != 200 {
		t.Fatalf("live fetch: %d %s", w.Code, w.Body.String())
	}
	var fetch domain.Fetch
	if err := json.Unmarshal(w.Body.Bytes(), &fetch); err != nil || len(fetch.Decisions) != 1 || fetch.Decisions[0].InterruptKey != key ||
		fetch.ExecutionID != fence.ExecutionID || fetch.Schema != domain.FetchSchema {
		t.Fatalf("fetch body = %s", w.Body.String())
	}
	// The body is the contract canonical form, so the digest the Worker
	// recomputes is over the same bytes Main wrote.
	if canonical, err := domain.Canonicalize(w.Body.Bytes(), 1<<20); err != nil || !bytes.Equal(canonical, w.Body.Bytes()) {
		t.Fatalf("fetch body is not canonical: %v", err)
	}
	if strings.Contains(w.Body.String(), foreignKey) {
		t.Fatal("fetch returned another response's card")
	}

	refusals := map[string]privateCall{
		"no client certificate":            {claimID: fence.ClaimID, fence: fence.FenceToken, execution: fence.ExecutionID},
		"user session, no certificate":     {claimID: fence.ClaimID, fence: fence.FenceToken, execution: fence.ExecutionID, userSession: true},
		"missing claim headers":            {execution: fence.ExecutionID, tlsState: h.claimTLS()},
		"stale fence":                      {claimID: fence.ClaimID, fence: bytes.Repeat([]byte{9}, 32), execution: fence.ExecutionID, tlsState: h.claimTLS()},
		"another execution's claim":        {claimID: otherFence.ClaimID, fence: otherFence.FenceToken, execution: fence.ExecutionID, tlsState: h.claimTLS()},
		"foreign workload identity":        {claimID: fence.ClaimID, fence: fence.FenceToken, execution: fence.ExecutionID},
		"claim of execution on other path": {claimID: fence.ClaimID, fence: fence.FenceToken, execution: otherFence.ExecutionID, tlsState: h.claimTLS()},
	}
	foreign, _ := url.Parse("spiffe://elitea.test/runtime/other-worker")
	foreignCall := refusals["foreign workload identity"]
	foreignCall.tlsState = verifiedPeer(&x509.Certificate{URIs: []*url.URL{foreign}})
	refusals["foreign workload identity"] = foreignCall
	for name, call := range refusals {
		if w := callPrivate(routes, "fetch", call); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), key) {
			t.Errorf("%s fetch: %d %s", name, w.Code, w.Body.String())
		}
	}
	checkpoint := "ckpt-http"
	ackBody, err := json.Marshal(domain.Ack{InterruptKey: key, RequestID: fetch.Decisions[0].RequestID, Revision: 2,
		DecisionSHA256: fetch.Decisions[0].DecisionSHA256, Outcome: domain.AckApplied, ChildCheckpointID: &checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	ack := live
	ack.body = ackBody
	for name, call := range refusals {
		call.body = ackBody
		if w := callPrivate(routes, "ack", call); w.Code != http.StatusForbidden {
			t.Errorf("%s ack: %d %s", name, w.Code, w.Body.String())
		}
	}
	if got := h.row(response, key); got.state != "DECIDED" {
		t.Fatalf("a refused ACK changed the row: %+v", got)
	}
	// NODE_RECOVERY claim of the same execution: refused, row unchanged.
	h.exec(`UPDATE elitea_runtime.execution_claims SET recovery_mode = 'NODE_RECOVERY' WHERE claim_id = $1`, fence.ClaimID)
	if w := callPrivate(routes, "ack", ack); w.Code != http.StatusForbidden {
		t.Fatalf("NODE_RECOVERY ack: %d", w.Code)
	}
	if w := callPrivate(routes, "fetch", live); w.Code != http.StatusForbidden {
		t.Fatalf("NODE_RECOVERY fetch: %d", w.Code)
	}
	h.exec(`UPDATE elitea_runtime.execution_claims SET recovery_mode = 'NONE' WHERE claim_id = $1`, fence.ClaimID)

	w = callPrivate(routes, "ack", ack)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"replay":false`) || !strings.Contains(w.Body.String(), `"state":"CONSUMED"`) {
		t.Fatalf("ACK: %d %s", w.Code, w.Body.String())
	}
	if w := callPrivate(routes, "ack", ack); w.Code != 200 || !strings.Contains(w.Body.String(), `"replay":true`) {
		t.Fatalf("identical ACK replay: %d %s", w.Code, w.Body.String())
	}
	conflicting := ack
	conflicting.body = bytes.Replace(ackBody, []byte("ckpt-http"), []byte("ckpt-other"), 1)
	if w := callPrivate(routes, "ack", conflicting); w.Code != http.StatusConflict {
		t.Fatalf("conflicting ACK replay: %d %s", w.Code, w.Body.String())
	}
	unknown := ack
	unknown.body = bytes.Replace(ackBody, []byte(key), []byte(strings.Repeat("cd", 32)), 1)
	if w := callPrivate(routes, "ack", unknown); w.Code != http.StatusNotFound {
		t.Fatalf("unknown key ACK: %d", w.Code)
	}
	foreignAck := ack
	foreignAck.body = bytes.Replace(ackBody, []byte(key), []byte(foreignKey), 1)
	if w := callPrivate(routes, "ack", foreignAck); w.Code != http.StatusNotFound || h.row(otherResponse, foreignKey).state != "DECIDED" {
		t.Fatalf("another response's card ACKed through this claim: %d", w.Code)
	}
	tooBig := ack
	tooBig.body = append(bytes.Repeat([]byte(" "), storage.MaxExecutionInterruptAckBytes), ackBody...)
	if w := callPrivate(routes, "ack", tooBig); w.Code != http.StatusBadRequest {
		t.Fatalf("ACK over 1024 bytes: %d", w.Code)
	}
	invalid := ack
	invalid.body = []byte(`{"interrupt_key":"` + key + `"}`)
	if w := callPrivate(routes, "ack", invalid); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid ACK body: %d", w.Code)
	}
}

// TestOldClaimAckRefusedAfterTakeover: a replacement claim (attempt 2, new
// fence) delivers the decision; the old claim's late ACK is refused and the
// replacement's ACK consumes the card.
func TestOldClaimAckRefusedAfterTakeover(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	old := h.bindLiveClaim(response)
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	if _, err := h.decide(interruptOwner, response, key, "takeover", "approve", ""); err != nil {
		t.Fatal(err)
	}
	fetch, err := h.repo.FetchDecided(t.Context(), old)
	if err != nil || len(fetch.Decisions) != 1 {
		t.Fatal(fetch, err)
	}
	h.exec(`UPDATE elitea_runtime.execution_claims SET released_at = clock_timestamp() WHERE claim_id = $1`, old.ClaimID)
	replacement := old
	replacement.ClaimID = old.ClaimID + "-2"
	replacement.FenceToken = bytes.Repeat([]byte{4}, 32)
	h.exec(`
INSERT INTO elitea_runtime.execution_claims (
    claim_id, execution_id, generation, workload_session_id, workload_identity, producer_id,
    claim_attempt, lease_epoch, fence_token, claimed_at, lease_expires_at
) VALUES ($1, $2, 1, $3, $4, 'rust-worker-1', 2, 2, $5, clock_timestamp(), clock_timestamp() + interval '10 minutes')`,
		replacement.ClaimID, old.ExecutionID, "session-"+old.ExecutionID, old.WorkloadIdentity, replacement.FenceToken)
	routes := h.privateRoutes()
	checkpoint := "ckpt-takeover"
	body, _ := json.Marshal(domain.Ack{InterruptKey: key, RequestID: fetch.Decisions[0].RequestID, Revision: 2,
		DecisionSHA256: fetch.Decisions[0].DecisionSHA256, Outcome: domain.AckApplied, ChildCheckpointID: &checkpoint})
	oldCall := privateCall{claimID: old.ClaimID, fence: old.FenceToken, execution: old.ExecutionID, tlsState: h.claimTLS(), body: body}
	if w := callPrivate(routes, "ack", oldCall); w.Code != http.StatusForbidden {
		t.Fatalf("old claim ACK: %d", w.Code)
	}
	newCall := oldCall
	newCall.claimID, newCall.fence = replacement.ClaimID, replacement.FenceToken
	newCall.body = nil
	w := callPrivate(routes, "fetch", newCall)
	if w.Code != 200 || !strings.Contains(w.Body.String(), key) {
		t.Fatalf("replacement fetch: %d %s", w.Code, w.Body.String())
	}
	newCall.body = body
	if w := callPrivate(routes, "ack", newCall); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"CONSUMED"`) {
		t.Fatalf("replacement ACK: %d %s", w.Code, w.Body.String())
	}
	if got := h.audit(response, key); got[len(got)-1] != "CONSUMED:3:claim="+replacement.ClaimID {
		t.Fatalf("audit = %v", got)
	}
}

// TestFetchAtSixteenEntries: a fetch returns all 16 DECIDED cards (the open
// cap); a seventeenth open card cannot be raised, so a 17th entry cannot
// exist (TestFetchAndListRefuseMoreThanTheCap proves a forced 17th row is a
// typed fault).
func TestFetchAtSixteenEntries(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	fence := h.bindLiveClaim(response)
	for i := range domain.MaxOpenInterrupts {
		key := h.raise(response, "fanout-interrupt-card-v1.json")
		if _, err := h.decide(interruptOwner, response, key, fmt.Sprintf("sixteen-%d", i), "approve", ""); err != nil {
			t.Fatal(err)
		}
	}
	key, raw := h.card("fanout-interrupt-card-v1.json", nil)
	if _, err := h.repo.Raise(t.Context(), h.raiseInput(response, raw)); !errors.Is(err, domain.ErrCapReached) {
		t.Fatalf("seventeenth open card %s: %v", key, err)
	}
	w := callPrivate(h.privateRoutes(), "fetch", privateCall{claimID: fence.ClaimID, fence: fence.FenceToken, execution: fence.ExecutionID, tlsState: h.claimTLS()})
	var fetch domain.Fetch
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &fetch) != nil || len(fetch.Decisions) != domain.MaxOpenInterrupts || fetch.DecisionRevision != 16 {
		t.Fatalf("fetch of 16: %d entries=%d", w.Code, len(fetch.Decisions))
	}
}

// TestFanoutWakeExecutionIsRecognized: the claim path's opt-in gate
// (claims.go) recognizes exactly the executions a wake row was admitted as.
func TestFanoutWakeExecutionIsRecognized(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.raise(response, "fanout-interrupt-card-v1.json")
	wakeExecution := h.newResponse()
	h.exec(`INSERT INTO elitea_runtime.execution_interrupt_wakes
(root_response_id, decision_revision, project_id, parked_execution_id, parked_generation, idempotency_key, state, wake_execution_id)
VALUES ($1::uuid, 1, 1, $2, 1, 'continue-interrupt/' || $1 || '/1', 'ADMITTED', $3)`, response, h.executions[response], h.executions[wakeExecution])
	err := h.repo.shared.WithinTx(t.Context(), interruptWriteTx, func(tx sqlExecutor) error {
		wake, err := isFanoutWakeExecution(t.Context(), tx, h.executions[wakeExecution])
		if err != nil || !wake {
			return fmt.Errorf("wake execution not recognized: %v %v", wake, err)
		}
		plain, err := isFanoutWakeExecution(t.Context(), tx, h.executions[response])
		if err != nil || plain {
			return fmt.Errorf("plain execution taken for a wake: %v %v", plain, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPrivateRouteLogsCarryNoValues: a refused and a failed private call log
// fixed reasons only, never a key, value, fence, claim header or URL.
func TestPrivateRouteLogsCarryNoValues(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	fence := h.bindLiveClaim(response)
	key := h.raise(response, "fanout-interrupt-card-v1.map-ask-user.json")
	const answer = "the private answer text"
	if _, err := h.decide(interruptOwner, response, key, "logs", "answer", answer); err != nil {
		t.Fatal(err)
	}
	var captured bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	routes := h.privateRoutes()
	call := privateCall{claimID: fence.ClaimID, fence: fence.FenceToken, execution: fence.ExecutionID, tlsState: h.claimTLS()}
	if w := callPrivate(routes, "fetch", call); w.Code != 200 || !strings.Contains(w.Body.String(), answer) {
		t.Fatalf("fetch: %d", w.Code)
	}
	// Corrupt the stored decision so the fetch hits a ledger fault and logs.
	h.exec(`ALTER TABLE elitea_runtime.execution_interrupts DROP CONSTRAINT execution_interrupts_decision_json_check`)
	h.exec(`UPDATE elitea_runtime.execution_interrupts SET decision_json = convert_to('{"x":"`+answer+`"}', 'UTF8') WHERE interrupt_key = $1`, key)
	if w := callPrivate(routes, "fetch", call); w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), answer) {
		t.Fatalf("fetch over a corrupt row: %d %s", w.Code, w.Body.String())
	}
	stale := call
	stale.fence = bytes.Repeat([]byte{3}, 32)
	_ = callPrivate(routes, "fetch", stale)
	logs := captured.String()
	if !strings.Contains(logs, "ledger_invariant") {
		t.Fatalf("the fault was not logged: %q", logs)
	}
	for _, secret := range []string{answer, key, fence.ClaimID, base64.RawURLEncoding.EncodeToString(fence.FenceToken), "/interrupts/", interruptWorkloadIdentity} {
		if strings.Contains(logs, secret) {
			t.Errorf("log carries %q: %s", secret, logs)
		}
	}
}

// TestAWakeThatParksAgainWithAHeldDecisionRecordsThePark covers a child with
// two cards: the wake admitted for card A's decision restores A but cannot
// apply it until B is decided, so it parks again having seen that same
// revision while A is still DECIDED. The park is recorded (no second wake at
// the same revision, no fault); B's decision then admits exactly one wake.
func TestAWakeThatParksAgainWithAHeldDecisionRecordsThePark(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	a := h.raise(response, "fanout-interrupt-card-v1.json")
	b := h.raise(response, "fanout-interrupt-card-v1.map-ask-user.json")
	if s, err := h.park(response, 0); err != nil || s.WakeQueued {
		t.Fatalf("first park = %+v, %v", s, err)
	}
	if _, err := h.decide(interruptOwner, response, a, "held-a", "approve", ""); err != nil {
		t.Fatal(err)
	}
	if got := h.wakes(response); len(got) != 1 || !strings.HasPrefix(got[0], "QUEUED:1:") {
		t.Fatalf("wake after A = %v", got)
	}
	// The wake is admitted as its own execution, then parks again with A held.
	wake := strings.Repeat("e", 32)
	h.seedExecution(wake)
	h.exec(`UPDATE elitea_runtime.execution_interrupt_wakes SET state = 'ADMITTED', wake_execution_id = $2
WHERE root_response_id = $1::uuid`, response, wake)
	h.executions[response] = wake
	if s, err := h.park(response, 1); err != nil || s.WakeQueued {
		t.Fatalf("a wake parking again with a held decision = %+v, %v", s, err)
	}
	if n := h.count(`SELECT count(*) FROM elitea_runtime.execution_interrupt_responses
WHERE root_response_id = $1::uuid AND parked_execution_id = $2`, response, wake); n != 1 {
		t.Fatal("the second park was not recorded")
	}
	if got := h.wakes(response); len(got) != 1 || !strings.HasPrefix(got[0], "DONE:1:") {
		t.Fatalf("wakes after the second park = %v", got)
	}
	if _, err := h.decide(interruptOwner, response, b, "held-b", "answer", "yes"); err != nil {
		t.Fatal(err)
	}
	if got := h.wakes(response); len(got) != 2 || !strings.HasPrefix(got[1], "QUEUED:2:") {
		t.Fatalf("wakes after B = %v", got)
	}
}
