package storage

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/browsersession"
)

func testBrowserViewClient(t *testing.T) (*BrowserViewClient, ed25519.PublicKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	// The transport is not under test: a client with one endpoint and no
	// connection, as NewBrowserViewClient would leave it before a dial.
	client := &BrowserViewClient{
		keyID: "key-1", key: private, requester: "spiffe://elitea/main", endpoints: map[string]browserViewEndpoint{"dns:browser": {}},
		now: func() time.Time { return time.UnixMilli(1_800_000_000_000) },
	}
	return client, public
}

func testBrowserViewSession() browsersession.Session {
	name := browsersession.NameFor("0123456789abcdef0123456789abcdef")
	return browsersession.Session{
		SessionID: name, ExecutionID: "0123456789abcdef0123456789abcdef", Generation: 4, TenantID: "1", ProjectID: 5,
		OwnerActorID: "7", SupervisorAudience: "dns:browser", State: browsersession.StateLive,
		Spec: browsersession.Spec{Revision: 1, Name: name, Engine: 1, CPUMillis: 1000, MemoryBytes: 1 << 30, WallSeconds: 600, MaxPages: 3},
	}
}

func TestBrowserViewGrantIsTheSupervisorsJobGrant(t *testing.T) {
	client, public := testBrowserViewClient(t)
	session := testBrowserViewSession()
	for _, control := range []bool{false, true} {
		grant, err := client.SignViewGrant(session, control)
		if err != nil {
			t.Fatal(err)
		}
		input := append([]byte(browserViewGrantDomain), make([]byte, 8)...)
		binary.BigEndian.PutUint64(input[len(browserViewGrantDomain):], uint64(len(grant.GetClaimsBytes())))
		input = append(input, grant.GetClaimsBytes()...)
		if grant.GetKeyId() != "key-1" || !ed25519.Verify(public, input, grant.GetSignature()) {
			t.Fatal("the signature does not verify under the job grant domain")
		}
		var claims runtimev1.SandboxJobGrantClaimsV1
		if err := proto.Unmarshal(grant.GetClaimsBytes(), &claims); err != nil {
			t.Fatal(err)
		}
		digest := browsersession.ViewDigest(session.Spec, control)
		if claims.GetRevision() != 1 || claims.GetCancelOnly() || claims.GetTenantId() != "1" || claims.GetProjectId() != 5 ||
			claims.GetExecutionId() != session.ExecutionID || claims.GetActivationId() != session.SessionID ||
			!bytes.Equal(claims.GetRequestDigest(), digest[:]) || claims.GetSubmitterWorkloadIdentity() != "spiffe://elitea/main" ||
			claims.GetAudience() != "dns:browser" || claims.GetGeneration() != 4 ||
			claims.GetExpiresAtUnixMillis()-claims.GetIssuedAtUnixMillis() != 30_000 {
			t.Fatalf("claims=%v", &claims)
		}
	}
	watch, _ := client.SignViewGrant(session, false)
	drive, _ := client.SignViewGrant(session, true)
	if bytes.Equal(watch.GetClaimsBytes(), drive.GetClaimsBytes()) {
		t.Fatal("a watch grant authorizes driving")
	}
}

func TestBrowserViewGrantRefusals(t *testing.T) {
	client, _ := testBrowserViewClient(t)
	for name, mutate := range map[string]func(*browsersession.Session){
		"an unconfigured supervisor": func(s *browsersession.Session) { s.SupervisorAudience = "dns:elsewhere" },
		"a spec of another session":  func(s *browsersession.Session) { s.Spec.Name = browsersession.NameFor("other") },
		"no generation":              func(s *browsersession.Session) { s.Generation = 0 },
		"no tenant":                  func(s *browsersession.Session) { s.TenantID = "" },
		"no project":                 func(s *browsersession.Session) { s.ProjectID = 0 },
		"a malformed spec":           func(s *browsersession.Session) { s.Spec.Revision = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			session := testBrowserViewSession()
			mutate(&session)
			if _, err := client.SignViewGrant(session, true); err == nil {
				t.Fatal("signed")
			}
		})
	}
}

func TestNewBrowserViewClientRefusesAnUnsafeConfiguration(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(nil)
	for name, cfg := range map[string]BrowserViewClientConfig{
		"no endpoints": {KeyID: "k", Key: private, Requester: "main"},
		"no key":       {KeyID: "k", Requester: "main", Endpoints: []BrowserViewEndpoint{{Audience: "a", Origin: "https://a:1"}}},
		"too fast":     {KeyID: "k", Key: private, Requester: "main", MaxFPS: 11, Endpoints: []BrowserViewEndpoint{{Audience: "a", Origin: "https://a:1"}}},
		"no TLS":       {KeyID: "k", Key: private, Requester: "main", Endpoints: []BrowserViewEndpoint{{Audience: "a", Origin: "https://a:1"}}},
		"plain origin": {KeyID: "k", Key: private, Requester: "main", Endpoints: []BrowserViewEndpoint{{Audience: "a", Origin: "http://a:1"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if client, err := NewBrowserViewClient(cfg); err == nil {
				client.Close()
				t.Fatal("accepted")
			}
		})
	}
}

// The supervisor's refusals map to the relay's kinds: the typed metadata
// first, then the gRPC code.
func TestBrowserViewErrorMapsTheSupervisorsRefusals(t *testing.T) {
	for _, test := range []struct {
		err   error
		typed string
		want  error
	}{
		{grpcstatus.Error(codes.NotFound, "x"), "browser_session_not_started", browsersession.ErrViewSessionGone},
		{grpcstatus.Error(codes.FailedPrecondition, "x"), "browser_session_ended", browsersession.ErrViewSessionGone},
		{grpcstatus.Error(codes.NotFound, "x"), "", browsersession.ErrViewSessionGone},
		{grpcstatus.Error(codes.ResourceExhausted, "x"), "browser_capacity", browsersession.ErrViewBusy},
		{grpcstatus.Error(codes.ResourceExhausted, "x"), "", browsersession.ErrViewBusy},
		{grpcstatus.Error(codes.PermissionDenied, "x"), "", browsersession.ErrViewRefused},
		{grpcstatus.Error(codes.InvalidArgument, "x"), "browser_ceiling_exceeded", browsersession.ErrViewRefused},
		{grpcstatus.Error(codes.FailedPrecondition, "x"), "browser_session_fenced", browsersession.ErrViewUnavailable},
		{grpcstatus.Error(codes.Unavailable, "x"), "", browsersession.ErrViewUnavailable},
		{grpcstatus.Error(codes.DeadlineExceeded, "x"), "", browsersession.ErrViewUnavailable},
		{io.EOF, "", browsersession.ErrViewUnavailable},
	} {
		if got := BrowserViewError(test.err, test.typed); !errors.Is(got, test.want) {
			t.Errorf("%v %q: %v, want %v", test.err, test.typed, got, test.want)
		}
	}
	if BrowserViewError(nil, "browser_capacity") != nil {
		t.Fatal("nil is not a refusal")
	}
}
