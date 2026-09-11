package toolkitdiscovery

import (
	"bytes"
	"context"
	"errors"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"testing"
	"time"
)

type dispatchStore struct {
	stored *executionapp.StoredPreparedEnvelope
	marks  int
}

func (s *dispatchStore) LoadPreparedToolkitAvailableTools(context.Context, string) (*executionapp.StoredPreparedEnvelope, error) {
	return s.stored, nil
}
func (s *dispatchStore) StorePreparedToolkitAvailableTools(_ context.Context, _ string, e executionapp.PreparedCommandEnvelope) (executionapp.StoredPreparedEnvelope, error) {
	s.stored = &executionapp.StoredPreparedEnvelope{Envelope: e.Clone()}
	return *s.stored, nil
}
func (s *dispatchStore) MarkToolkitAvailableToolsPublished(_ context.Context, _ string, d runtime.Digest) error {
	if s.stored.Envelope.Digest != d {
		return errors.New("wrong envelope")
	}
	s.marks++
	return nil
}

type dispatchProducer struct {
	prepared int
	appended [][]byte
	fail     bool
}

func (p *dispatchProducer) PrepareToolkitAvailableTools(context.Context, Dispatch) (executionapp.PreparedCommandEnvelope, error) {
	p.prepared++
	data := []byte("immutable-signed-command")
	return executionapp.PreparedCommandEnvelope{Bytes: data, Digest: runtime.SHA256(data), SignatureProfile: 1, KeyID: "key"}, nil
}
func (p *dispatchProducer) AppendPrepared(_ context.Context, _ string, e executionapp.PreparedCommandEnvelope) error {
	p.appended = append(p.appended, bytes.Clone(e.Bytes))
	if p.fail {
		p.fail = false
		return errors.New("uncertain Redis acknowledgement")
	}
	return nil
}
func TestDiscoveryDispatchRetriesExactDurableBytes(t *testing.T) {
	store := &dispatchStore{}
	producer := &dispatchProducer{fail: true}
	dispatcher, _ := NewDispatcher(store, producer)
	dispatch := Dispatch{OutboxID: "outbox", CommandID: "command", ExecutionID: "execution", Generation: 1, DispatchOrdinal: 1, TenantID: "7", ResourceProjectID: "7", ProjectionProjectID: "7", PrincipalRef: "42", InputBundleID: "bundle", InputBundleVersion: "version", InputBundleMediaType: domain.InputBundleManifestMediaType, InputBundleByteLength: 1, InputBundleDigest: runtime.SHA256([]byte("bundle")), CapabilityID: domain.ToolkitAvailableToolsCapability, CapabilityVersion: "1", ResourceClass: "indexing", IsolationClass: "project", Priority: 1, Deadline: time.Now().Add(time.Hour), LimitsRevision: "limits", ToolkitType: "github", SettingsEntryID: SettingsEntryID}
	if dispatcher.Dispatch(context.Background(), dispatch) == nil || store.stored == nil || store.marks != 0 {
		t.Fatal("uncertain publish was marked or intent lost")
	}
	if err := dispatcher.Dispatch(context.Background(), dispatch); err != nil {
		t.Fatal(err)
	}
	if producer.prepared != 1 || len(producer.appended) != 2 || !bytes.Equal(producer.appended[0], producer.appended[1]) || store.marks != 1 {
		t.Fatal("retry changed selected command bytes")
	}
	store.stored.Envelope.Bytes[0] = '!'
	if err := dispatcher.Dispatch(context.Background(), dispatch); !errors.Is(err, executionapp.ErrInvalidPreparedEnvelope) || len(producer.appended) != 2 {
		t.Fatal("corrupt envelope reached Redis")
	}
}
