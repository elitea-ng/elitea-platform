package codeplatform

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
)

type callJournalFixture struct {
	record                                              domain.Record
	found                                               bool
	begins, dispatches, commits, reconciles, signChecks int
	denied                                              bool
}

func (j *callJournalFixture) Lookup(ctx context.Context, _ domain.Admission, _ uint64) (domain.Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Record{}, false, err
	}
	if j.denied {
		return domain.Record{}, false, domain.ErrUnauthorized
	}
	return j.record, j.found, nil
}
func (j *callJournalFixture) Begin(_ context.Context, a domain.Admission, i domain.Intent) (domain.Record, error) {
	j.begins++
	j.record = domain.Record{Intent: i, EffectID: domain.EffectID(a.Job, i), State: "prepared"}
	j.found = true
	return j.record, nil
}
func (j *callJournalFixture) Dispatch(_ context.Context, _ domain.Admission, _ domain.Record) (bool, error) {
	if j.record.State != "prepared" {
		return false, domain.ErrUnknown
	}
	j.dispatches++
	j.record.State = "dispatching"
	return true, nil
}
func (j *callJournalFixture) Commit(_ context.Context, _ domain.Admission, r domain.Record) error {
	j.commits++
	j.record = r
	return nil
}
func (j *callJournalFixture) CommitCodeToolkitReconciliation(_ context.Context, _ domain.Admission, r domain.Record) error {
	j.reconciles++
	j.record = r
	return nil
}
func (j *callJournalFixture) VerifyCommittedCodeCall(_ context.Context, _ domain.Admission, r domain.Record) error {
	j.signChecks++
	if j.denied {
		return domain.ErrUnauthorized
	}
	if !j.record.Committed() || j.record.Response != r.Response {
		return domain.ErrConflict
	}
	return nil
}

type privateContentFixture struct {
	values       map[string][]byte
	puts         int
	failResponse bool
}

func (c *privateContentFixture) Put(_ context.Context, _ domain.ContentBinding, kind string, raw []byte) (string, error) {
	if kind == "response" && c.failResponse {
		return "", domain.ErrUnavailable
	}
	c.puts++
	ref := fmt.Sprintf("%064x.cp1", c.puts)
	c.values[ref] = append([]byte(nil), raw...)
	return ref, nil
}
func (c *privateContentFixture) Read(_ context.Context, _ domain.ContentBinding, _ string, ref string) ([]byte, error) {
	raw, found := c.values[ref]
	if !found {
		return nil, domain.ErrUnavailable
	}
	return append([]byte(nil), raw...), nil
}

type operationFixture struct {
	executed, reconciled, replyChecks int
	known, reconcileKnown             bool
	fail, replyDenied                 bool
}

func (o *operationFixture) AuthorizeReply(ctx context.Context, _ domain.Admission, _ domain.Record, _ domain.Request, _ domain.Reply) error {
	o.replyChecks++
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.replyDenied {
		return domain.ErrUnauthorized
	}
	return nil
}
func (o *operationFixture) Execute(_ context.Context, _ domain.Admission, _ domain.Record, _ domain.Request) (Outcome, error) {
	o.executed++
	if o.fail {
		return Outcome{}, domain.ErrUnavailable
	}
	return Outcome{Status: "ok", Result: json.RawMessage(`{"value":"exact"}`), Payload: []byte{0, 255, 13, 10}, Known: o.known}, nil
}
func (o *operationFixture) Reconcile(_ context.Context, _ domain.Admission, _ domain.Record, _ domain.Request) (Outcome, bool, error) {
	o.reconciled++
	return Outcome{Status: "ok", Result: json.RawMessage(`{"value":"original child"}`), Known: o.reconcileKnown, OwnerReceipt: "native-owner-receipt"}, o.reconcileKnown, nil
}

type signerFixture struct{}

func (signerFixture) SignCommittedCodeCall(ctx context.Context, authority domain.SigningAuthority, a domain.Admission, r domain.Record) (domain.SignedReply, error) {
	if err := authority.VerifyCommittedCodeCall(ctx, a, r); err != nil {
		return domain.SignedReply{}, err
	}
	claims, err := domain.ReplyClaims("main-current", a.Job, r)
	if err != nil {
		return domain.SignedReply{}, err
	}
	raw, _ := json.Marshal(claims)
	return domain.SignedReply{Revision: 1, KeyID: "main-current", Claims: raw, Signature: "component-fixture-signature"}, nil
}
func serviceFixture(t *testing.T) (*Service, *callJournalFixture, *privateContentFixture, *operationFixture, domain.Admission) {
	t.Helper()
	j := &callJournalFixture{}
	c := &privateContentFixture{values: map[string][]byte{}}
	o := &operationFixture{known: true}
	s, err := NewService(j, c, o, signerFixture{})
	if err != nil {
		t.Fatal(err)
	}
	a := domain.Admission{Job: domain.Job{TenantID: "tenant", ExecutionID: "0123456789abcdef0123456789abcdef", OriginalGeneration: 1, Activation: [32]byte{1}, PreparedRequest: [32]byte{2}, Policy: [32]byte{3}, RuntimeID: "original-runtime", ProjectID: 7, ActorID: 11, MaxCalls: 32, MaxTotalBytes: 1048576}, ClaimID: "current-claim", Generation: 1, LeaseEpoch: 1, WorkloadIdentity: "spiffe://elitea/worker/one", FenceToken: []byte{1}}
	return s, j, c, o, a
}
func callFrame(operation string) []byte {
	resource := `{"kind":"current_user"}`
	if operation == "toolkit_call" {
		resource = `{"kind":"toolkit","id":"17","revision":"` + fmt.Sprintf("%064x", 1) + `","tool":"echo"}`
	}
	header := []byte(`{"revision":1,"sequence":1,"operation":"` + operation + `","resource":` + resource + `,"arguments":{}}`)
	frame := make([]byte, 8+len(header))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(header)))
	copy(frame[8:], header)
	return frame
}
func TestCodeCallCommittedReplayReturnsExactBytesWithoutRedispatch(t *testing.T) {
	s, j, _, o, a := serviceFixture(t)
	frame := callFrame("user_get")
	first, err := s.Call(context.Background(), a, frame)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Call(context.Background(), a, frame)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || o.executed != 1 || j.dispatches != 1 || j.commits != 1 || j.signChecks != 2 {
		t.Fatal("replay changed bytes or repeated an operation")
	}
	if j.record.Intent.FrameBytes != uint64(len(frame)) || !j.record.Committed() {
		t.Fatal("original receipt missing")
	}
}
func TestUnknownCodeCallNeverRedispatchesAndForeignFrameIsRefused(t *testing.T) {
	s, j, _, o, a := serviceFixture(t)
	o.known = false
	frame := callFrame("user_get")
	for range 2 {
		if _, err := s.Call(context.Background(), a, frame); !errors.Is(err, domain.ErrUnknown) {
			t.Fatalf("unknown result %v", err)
		}
	}
	if o.executed != 1 || j.dispatches != 1 || j.commits != 0 {
		t.Fatal("unknown effect retried")
	}
	foreign := append(append([]byte(nil), frame...), ' ')
	binary.BigEndian.PutUint32(foreign[:4], uint32(len(foreign)-8))
	if _, err := s.Call(context.Background(), a, foreign); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("different exact valid frame was not refused as identity conflict")
	}
	if o.executed != 1 {
		t.Fatal("foreign frame dispatched")
	}
}
func TestCodeCallResponseStorageFailureKeepsOriginalUnknownEffect(t *testing.T) {
	s, j, c, o, a := serviceFixture(t)
	c.failResponse = true
	frame := callFrame("user_get")
	if _, err := s.Call(context.Background(), a, frame); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatal(err)
	}
	c.failResponse = false
	if _, err := s.Call(context.Background(), a, frame); !errors.Is(err, domain.ErrUnknown) {
		t.Fatal(err)
	}
	if o.executed != 1 || j.commits != 0 || j.record.State != "dispatching" {
		t.Fatal("unknown effect became fresh dispatch")
	}
}
func TestCodeToolkitReconciliationUsesOriginalChildOnly(t *testing.T) {
	s, j, _, o, a := serviceFixture(t)
	o.known = false
	frame := callFrame("toolkit_call")
	if _, err := s.Call(context.Background(), a, frame); !errors.Is(err, domain.ErrUnknown) {
		t.Fatal(err)
	}
	if _, err := s.Call(context.Background(), a, frame); !errors.Is(err, domain.ErrUnknown) {
		t.Fatal(err)
	}
	o.reconcileKnown = true
	if _, err := s.Call(context.Background(), a, frame); err != nil {
		t.Fatal(err)
	}
	if o.executed != 1 || o.reconciled != 2 || j.reconciles != 1 || j.dispatches != 1 {
		t.Fatal("reconciliation submitted another child")
	}
}
func TestCodeCommittedCallCannotDeliverAfterCurrentAuthorityIsLost(t *testing.T) {
	s, j, _, o, a := serviceFixture(t)
	frame := callFrame("user_get")
	if _, err := s.Call(context.Background(), a, frame); err != nil {
		t.Fatal(err)
	}
	j.denied = true
	if _, err := s.Call(context.Background(), a, frame); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("stale committed reply delivered")
	}
	if o.executed != 1 {
		t.Fatal("revoked authority repeated effect")
	}
}

func TestCodeToolkitNeverSettlesRemainsOneOwnedEffect(t *testing.T) {
	s, j, _, o, a := serviceFixture(t)
	o.known = false
	frame := callFrame("toolkit_call")
	for range 8 {
		if _, err := s.Call(context.Background(), a, frame); !errors.Is(err, domain.ErrUnknown) {
			t.Fatal(err)
		}
	}
	if o.executed != 1 || o.reconciled != 7 || j.begins != 1 || j.dispatches != 1 || j.commits != 0 || j.reconciles != 0 {
		t.Fatal("pending child or call was resubmitted")
	}
}
func TestCodeToolkitClaimLossStopsFurtherOriginalChildObservations(t *testing.T) {
	s, j, _, o, a := serviceFixture(t)
	o.known = false
	frame := callFrame("toolkit_call")
	if _, err := s.Call(context.Background(), a, frame); !errors.Is(err, domain.ErrUnknown) {
		t.Fatal(err)
	}
	j.denied = true
	o.reconcileKnown = true
	if _, err := s.Call(context.Background(), a, frame); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("lost claim continued IO")
	}
	if o.executed != 1 || o.reconciled != 0 || j.reconciles != 0 {
		t.Fatal("lost claim observed or redispatched child")
	}
}
func TestCodePlatformStopDoesNotDispatchOrObserveAnotherEffect(t *testing.T) {
	s, j, _, o, a := serviceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Call(ctx, a, callFrame("toolkit_call")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if o.executed != 0 || o.reconciled != 0 || j.begins != 0 || j.dispatches != 0 {
		t.Fatal("Stop started owned work")
	}
}

func TestCodeCommittedReplyRechecksResourceSharingWithoutRepeatingEffect(t *testing.T) {
	s, j, _, o, a := serviceFixture(t)
	frame := callFrame("user_get")
	if _, err := s.Call(t.Context(), a, frame); err != nil {
		t.Fatal(err)
	}
	o.replyDenied = true
	if _, err := s.Call(t.Context(), a, frame); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("cached resource data delivered after sharing was revoked")
	}
	if o.executed != 1 || j.dispatches != 1 || j.commits != 1 || j.signChecks != 1 || o.replyChecks != 2 {
		t.Fatal("authorization repeated an effect or signed revoked data")
	}
}
