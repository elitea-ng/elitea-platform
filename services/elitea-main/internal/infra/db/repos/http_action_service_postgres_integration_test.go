package repos

// End-to-end Main HTTP action path on real PostgreSQL: the runtime-context
// route (mTLS claim headers), the real claim read, rule, RBAC, configuration
// and effect SQL, the egress guard transport (HTTPS, loopback named by the
// operator allowlist), and a counting HTTPS server. The Worker side (journal,
// checkpoint, process loss) arrives with 5d-2; here a Worker replacement is a
// new claim re-posting the same invocation.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const httpServicePipeline = `entry_point: fetch
nodes:
  - id: fetch
    type: http
    revision: 2
    operation: tracker.item
    request:
      path: {item: {from: k}}
      body: {kind: json, from: payload}
    response: {mode: json}
    output: [out]
    transition: END
`

type sentinelUnsecreter struct{}

func (sentinelUnsecreter) Unsecret(_ context.Context, _ int32, data map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for key, value := range data {
		if text, ok := value.(string); ok {
			value = strings.ReplaceAll(text, "{{secret.k}}", "sentinel-secret")
		}
		out[key] = value
	}
	return out, nil
}

type httpServiceWorld struct {
	*httpWorld
	remote      *httptest.Server
	remoteCalls atomic.Int32
	remoteAuth  sync.Map
	server      *storage.ContentServer
	logs        *bytes.Buffer
	snapshot    *app.FrozenSnapshot
	thread      string
}

func newHTTPServiceWorld(t *testing.T) *httpServiceWorld {
	t.Helper()
	w := &httpServiceWorld{httpWorld: newHTTPWorld(t, "POST"), logs: &bytes.Buffer{}}
	w.remote = httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.remoteCalls.Add(1)
		w.remoteAuth.Store(r.URL.EscapedPath(), r.Header.Get("Authorization"))
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusCreated)
		_, _ = rw.Write([]byte(`{"created":"sentinel-response"}`))
	}))
	t.Cleanup(w.remote.Close)
	origin, _ := url.Parse(w.remote.URL)
	w.exec(`UPDATE elitea_runtime.http_action_rules SET origin=$1 WHERE id=$2`, "https://"+origin.Host, w.ruleID)
	snapshot, err := app.FreezeSnapshot(31, 41, httpServicePipeline)
	if err != nil {
		t.Fatal(err)
	}
	w.snapshot = snapshot
	application, _ := json.Marshal(map[string]any{"id": 31, "version_id": 41, "version_details": map[string]any{"instructions": httpServicePipeline, "http_action_snapshot": snapshot}})
	payload, _ := proto.Marshal(&runtimev1.AgentExecutionInputV1{ThreadId: proto.String("root-thread"), Application: application, ToolkitGuardrails: []byte(`{}`)})
	w.exec(`UPDATE elitea_runtime.input_bundle_entries SET content_bytes=$1, content_size=$2 WHERE input_bundle_id=$3`, payload, len(payload), "bundle-"+httpWorldExecution)
	w.thread = app.RootGraphThread("1", 1, 1, "root-thread")

	guard, err := egress.NewHTTPActionGuard("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	transport := guard.Transport()
	transport.TLSClientConfig = w.remote.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	credentials, err := NewHTTPActionCredentials(w.pool, sentinelUnsecreter{}, transport)
	if err != nil {
		t.Fatal(err)
	}
	service, err := storage.NewRuntimeHTTPActionService(w.repo, w.repo, w.repo, credentials, nil, transport, slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	content, err := storage.NewPostgresContentRepository(w.pool)
	if err != nil {
		t.Fatal(err)
	}
	server, err := storage.NewContentServer(content, content, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.server = server.WithRuntimeHTTPActions(service)
	return w
}

func (w *httpServiceWorld) invocation(step uint64, item string) []byte {
	inputs, _ := app.CanonicalJSON([]byte(`{"body":{"note":"sentinel-body"},"path":{"item":"` + item + `"}}`))
	inv := app.Invocation{SchemaVersion: app.Schema, NodeID: "fetch", ThreadID: w.thread, Step: step, BindingDigest: w.snapshot.Nodes[0].BindingDigest, InputsB64: base64.StdEncoding.EncodeToString(inputs), InputsDigest: app.Digest(inputs)}
	inv.ActivationID = app.VisitID(inv.ThreadID, inv.NodeID, inv.Step, app.BindingBytes(inv.BindingDigest))
	body, _ := json.Marshal(inv)
	return body
}

func (w *httpServiceWorld) post(claim string, fence byte, body []byte) ([]byte, error) {
	identity, _ := url.Parse(effectsWorkload)
	certificate := &x509.Certificate{URIs: []*url.URL{identity}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{certificate}}, PeerCertificates: []*x509.Certificate{certificate}}},
	})
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(
		grpcauth.MetadataClaimID, claim,
		grpcauth.MetadataClaimFence, string(bytes.Repeat([]byte{fence}, 32)),
	))
	response, err := w.server.ExecuteHTTPAction(ctx, &runtimev1.ExecuteHTTPActionRequestV1{
		Scope:          &runtimev1.RuntimeContentScopeV1{ExecutionId: httpWorldExecution, Generation: 1},
		InvocationJson: body,
	})
	if err != nil {
		return nil, err
	}
	return response.GetReceiptJson(), nil
}

// Design §13 case 1 (Main side): after Main committed the receipt, a Worker
// replacement (new claim) re-posts the same invocation and gets the stored
// receipt; the remote server sees exactly one request.
func TestHTTPActionRouteWorkerReplacementGetsStoredReceiptPostgres(t *testing.T) {
	w := newHTTPServiceWorld(t)
	body := w.invocation(1, "a/b")
	first, err := w.post(httpWorldClaim, 0x11, body)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	var receipt app.Receipt
	if err := json.Unmarshal(first, &receipt); err != nil || receipt.State != "completed" || receipt.Result.Status != 201 {
		t.Fatalf("receipt %s", first)
	}
	if auth, _ := w.remoteAuth.Load("/items/a%2Fb"); auth != "Bearer sentinel-secret" || w.remoteCalls.Load() != 1 {
		t.Fatalf("remote saw %v calls=%d", auth, w.remoteCalls.Load())
	}
	w.exec(`UPDATE elitea_runtime.execution_claims SET released_at=clock_timestamp(), release_reason='takeover' WHERE claim_id=$1`, httpWorldClaim)
	w.access(httpWorldExecution, 1, httpWorldClaim2, 2, 0x12)
	// The released claim is refused before any work.
	if _, err := w.post(httpWorldClaim, 0x11, body); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("released claim answered %v", err)
	}
	second, err := w.post(httpWorldClaim2, 0x12, body)
	if err != nil || !bytes.Equal(second, first) || w.remoteCalls.Load() != 1 {
		t.Fatalf("replacement: %v calls=%d", err, w.remoteCalls.Load())
	}
}

// Log/event capture: secrets, inputs, URLs and bodies never reach logs or
// receipts, on success and on every refusal path.
func TestHTTPActionRouteLogsCarryNoSensitiveFieldsPostgres(t *testing.T) {
	w := newHTTPServiceWorld(t)
	// The success receipt carries the response projection by design; it must
	// not carry the credential, the request inputs or the destination.
	success, err := w.post(httpWorldClaim, 0x11, w.invocation(1, "sentinel-path"))
	if err != nil {
		t.Fatalf("success call: %v", err)
	}
	origin, _ := url.Parse(w.remote.URL)
	for _, sentinel := range []string{"sentinel-secret", "sentinel-body", "sentinel-path", origin.Host, "Bearer"} {
		if bytes.Contains(success, []byte(sentinel)) {
			t.Fatalf("%q reached the receipt", sentinel)
		}
	}
	responses := [][]byte{}
	w.exec(`UPDATE elitea_runtime.http_action_rules SET enabled=FALSE, revision=revision+1 WHERE id=$1`, w.ruleID)
	_, deniedErr := w.post(httpWorldClaim, 0x11, w.invocation(2, "sentinel-path"))
	if grpcstatus.Code(deniedErr) != codes.PermissionDenied {
		t.Fatalf("disabled rule answered %v", deniedErr)
	}
	_, badErr := w.post(httpWorldClaim, 0x11, []byte(`{"bad":"sentinel-garbage"}`))
	responses = append(responses, []byte(deniedErr.Error()), []byte(badErr.Error()))
	captured := append(bytes.Join(responses, nil), w.logs.Bytes()...)
	for _, sentinel := range []string{"sentinel-secret", "sentinel-body", "sentinel-path", "sentinel-response", "sentinel-garbage", origin.Host, "Bearer"} {
		if bytes.Contains(captured, []byte(sentinel)) {
			t.Fatalf("%q reached logs or responses", sentinel)
		}
	}
	if !bytes.Contains(w.logs.Bytes(), []byte(`"http action"`)) || !bytes.Contains(w.logs.Bytes(), []byte(`"http action refused"`)) {
		t.Fatalf("expected safe action log lines, got %s", w.logs)
	}
}
