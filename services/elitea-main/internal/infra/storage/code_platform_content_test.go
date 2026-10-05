package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
)

type codeContentMemory struct {
	ObjectStore
	objects map[ObjectRef][]byte
	writes  int
}

func (m *codeContentMemory) Put(_ context.Context, ref ObjectRef, body io.Reader, options PutOptions) (ObjectInfo, error) {
	raw, err := io.ReadAll(io.LimitReader(body, options.ContentLength+1))
	if err != nil || int64(len(raw)) != options.ContentLength {
		return ObjectInfo{}, domain.ErrConflict
	}
	m.objects[ref] = raw
	m.writes++
	return ObjectInfo{Size: int64(len(raw))}, nil
}
func (m *codeContentMemory) Get(_ context.Context, ref ObjectRef, _ *ByteRange) (io.ReadCloser, ObjectInfo, error) {
	raw, ok := m.objects[ref]
	if !ok {
		return nil, ObjectInfo{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(raw)), ObjectInfo{Size: int64(len(raw))}, nil
}
func contentKeys(t *testing.T, current string, ids ...string) *CodePlatformContentKeyring {
	t.Helper()
	keys := make([]map[string]string, 0, len(ids))
	for i, id := range ids {
		keys = append(keys, map[string]string{"id": id, "key_base64url": base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{byte(i + 1)}, 32))})
	}
	raw, err := json.Marshal(map[string]any{"revision": 1, "current_key_id": current, "keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	ring, err := ParseCodePlatformContentKeys(raw)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}
func contentBinding() domain.ContentBinding {
	return domain.ContentBinding{Job: domain.Job{TenantID: "tenant-a", ExecutionID: "0123456789abcdef0123456789abcdef", OriginalGeneration: 1, Activation: [32]byte{1}, PreparedRequest: [32]byte{2}, Policy: [32]byte{3}, RuntimeID: strings.Repeat("4", 64), ProjectID: 7, ActorID: 9, MaxCalls: 4, MaxTotalBytes: 1048576}, Sequence: 1, Operation: "secret_read", Resource: [32]byte{5}, Request: [32]byte{6}}
}
func TestCodeContentKeyringRejectsMalformedDuplicateAndUnknownKeys(t *testing.T) {
	key := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	entry := `{"id":"current","key_base64url":"` + key + `"}`
	valid := `{"revision":1,"current_key_id":"current","keys":[` + entry + `]}`
	cases := []string{"", `null`, strings.Replace(valid, `"revision":1`, `"revision":1,"revision":1`, 1), strings.Replace(valid, `"current_key_id":"current"`, `"current_key_id":"absent"`, 1), strings.Replace(valid, entry, entry+","+entry, 1), strings.Replace(valid, key, "bad", 1), strings.Replace(valid, `"id":"current"`, `"id":"../current"`, 1), strings.Replace(valid, key, base64.RawURLEncoding.EncodeToString(make([]byte, 32)), 1), strings.Replace(valid, `"keys":`, `"unexpected":true,"keys":`, 1), strings.Repeat(" ", CodePlatformContentKeyFileLimit+1)}
	for _, raw := range cases {
		if _, err := ParseCodePlatformContentKeys([]byte(raw)); err == nil {
			t.Fatalf("accepted malformed keyring %q", raw[:min(len(raw), 80)])
		}
	}
	ring, err := ParseCodePlatformContentKeys([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ring.ResolveCodePlatformKey(context.Background(), "unknown"); err == nil {
		t.Fatal("unknown key accepted")
	}
}
func TestCodeContentExactBytesRotationAndRetirement(t *testing.T) {
	ctx := context.Background()
	store := &codeContentMemory{objects: make(map[ObjectRef][]byte)}
	binding := contentBinding()
	data := []byte{0, 255, 128, 1, 10}
	old, err := NewCodePlatformContent(store, contentKeys(t, "old", "old"))
	if err != nil {
		t.Fatal(err)
	}
	reference, err := old.Put(ctx, binding, "intent", data)
	if err != nil {
		t.Fatal(err)
	}
	next, err := NewCodePlatformContent(store, contentKeys(t, "new", "old", "new"))
	if err != nil {
		t.Fatal(err)
	}
	actual, err := next.Read(ctx, binding, "intent", reference)
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("retained read changed exact bytes", err)
	}
	fresh, err := next.Put(ctx, binding, "intent", data)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == reference {
		t.Fatal("reference or nonce reused")
	}
	retiredKeys, err := ParseCodePlatformContentKeys([]byte(`{"revision":1,"current_key_id":"new","keys":[{"id":"new","key_base64url":"` + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	retired, err := NewCodePlatformContent(store, retiredKeys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = retired.Read(ctx, binding, "intent", reference); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatal("retired key still readable", err)
	}
	if actual, err = retired.Read(ctx, binding, "intent", fresh); err != nil || !bytes.Equal(actual, data) {
		t.Fatal("current key unreadable", err)
	}
}
func TestCodeContentRefusesTamperingCrossOwnerAndCallSubstitution(t *testing.T) {
	ctx := context.Background()
	store := &codeContentMemory{objects: make(map[ObjectRef][]byte)}
	binding := contentBinding()
	content, err := NewCodePlatformContent(store, contentKeys(t, "current", "current"))
	if err != nil {
		t.Fatal(err)
	}
	reference, err := content.Put(ctx, binding, "intent", []byte("exact secret intent"))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := codeContentRef(binding.Job, reference)
	original := append([]byte(nil), store.objects[ref]...)
	changes := []func(*domain.ContentBinding){func(b *domain.ContentBinding) { b.Job.TenantID = "tenant-b" }, func(b *domain.ContentBinding) { b.Job.ActorID++ }, func(b *domain.ContentBinding) { b.Job.OriginalGeneration++ }, func(b *domain.ContentBinding) { b.Job.RuntimeID = strings.Repeat("9", 64) }, func(b *domain.ContentBinding) { b.Job.Activation[0]++ }, func(b *domain.ContentBinding) { b.Job.PreparedRequest[0]++ }, func(b *domain.ContentBinding) { b.Job.Policy[0]++ }, func(b *domain.ContentBinding) { b.Sequence++ }, func(b *domain.ContentBinding) { b.Operation = "toolkit_call" }, func(b *domain.ContentBinding) { b.Resource[0]++ }, func(b *domain.ContentBinding) { b.Request[0]++ }}
	for i, change := range changes {
		candidate := binding
		change(&candidate)
		if _, err = content.Read(ctx, candidate, "intent", reference); err == nil {
			t.Fatalf("accepted substitution %d", i)
		}
	}
	project := binding
	project.Job.ProjectID++
	otherRef, _ := codeContentRef(project.Job, reference)
	store.objects[otherRef] = original
	if _, err = content.Read(ctx, project, "intent", reference); err == nil {
		t.Fatal("cross-project ciphertext accepted")
	}
	if _, err = content.Read(ctx, binding, "response", reference); err == nil {
		t.Fatal("schema substitution accepted")
	}
	otherReference := strings.Repeat("b", 64) + ".cp1"
	otherRef, _ = codeContentRef(binding.Job, otherReference)
	store.objects[otherRef] = original
	if _, err = content.Read(ctx, binding, "intent", otherReference); err == nil {
		t.Fatal("reference substitution accepted")
	}
	for _, position := range []int{0, 2, 4, len(original) - 1} {
		changed := append([]byte(nil), original...)
		changed[position] ^= 1
		store.objects[ref] = changed
		if _, err = content.Read(ctx, binding, "intent", reference); err == nil {
			t.Fatalf("accepted tamper at %d", position)
		}
	}
	store.objects[ref] = original[:len(original)-1]
	if _, err = content.Read(ctx, binding, "intent", reference); err == nil {
		t.Fatal("partial ciphertext accepted")
	}
	before := store.writes
	if _, err = content.Put(ctx, binding, "intent", make([]byte, codeContentBound("intent")+1)); err == nil || store.writes != before {
		t.Fatal("oversized content reached storage")
	}
}
