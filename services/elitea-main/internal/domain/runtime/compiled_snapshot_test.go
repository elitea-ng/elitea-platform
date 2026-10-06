package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func snapshotFixture(t *testing.T) (RustSnapshotBinding, []byte, string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/compiled-snapshot-v1/binding.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseRustSnapshotBinding(raw)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := os.ReadFile("testdata/compiled-snapshot-v1/descriptor.json")
	if err != nil {
		t.Fatal(err)
	}
	return b, descriptor, SnapshotContentSHA256(descriptor)
}
func TestCompiledSnapshotRunnerGoldenIdentity(t *testing.T) {
	b, descriptor, root := snapshotFixture(t)
	key, err := b.Key()
	if err != nil || key != "7942af2611b0d1c1aefbaee4119dc76a4a525a1621c220c0e63e42b3a610b912" || root != "1a8e8bd0fddd0c1d3dcefae672d81237ac80b4ca60ad60f7868cca29aa058658" {
		t.Fatal(key, root, err)
	}
	if _, err := ParseRustSnapshotDescriptor(descriptor, root); err != nil {
		t.Fatal(err)
	}
}
func TestCompiledSnapshotBindingRejectsNonCanonicalJSON(t *testing.T) {
	b, _, _ := snapshotFixture(t)
	raw, _ := SnapshotJSON(b)
	for _, bad := range [][]byte{append(append([]byte(nil), raw...), 10), []byte(strings.Replace(string(raw), `"revision":1`, `"revision":1,"revision":1`, 1)), []byte(strings.Replace(string(raw), `"revision":1`, `"revision":1,"extra":0`, 1)), append([]byte(" "), raw...), bytes.Repeat([]byte("x"), SnapshotDescriptorLimit+1)} {
		if _, err := ParseRustSnapshotBinding(bad); err == nil {
			t.Fatal("accepted malformed binding")
		}
	}
}
func TestCompiledSnapshotDescriptorRejectsSubstitutionAndSize(t *testing.T) {
	_, raw, root := snapshotFixture(t)
	d, _ := ParseRustSnapshotDescriptor(raw, root)
	for _, change := range []func(*RustSnapshotDescriptor){func(d *RustSnapshotDescriptor) { d.ExecutableBytes = 0 }, func(d *RustSnapshotDescriptor) { d.ExecutableBytes = SnapshotExecutableLimit + 1 }, func(d *RustSnapshotDescriptor) { d.SnapshotKeySHA256 = strings.Repeat("a", 64) }, func(d *RustSnapshotDescriptor) { d.Binding.TenantID = "other" }} {
		candidate := d
		change(&candidate)
		native, _ := SnapshotJSON(candidate)
		if _, err := ParseRustSnapshotDescriptor(native, SnapshotContentSHA256(native)); err == nil {
			t.Fatal("accepted substitute descriptor")
		}
	}
	if _, err := ParseRustSnapshotDescriptor(raw, strings.Repeat("a", 64)); err == nil {
		t.Fatal("accepted wrong root")
	}
}
func TestCompiledSnapshotAllProfileFieldsAffectKey(t *testing.T) {
	b, _, _ := snapshotFixture(t)
	key, _ := b.Key()
	changes := []func(*RustSnapshotBinding){func(b *RustSnapshotBinding) { b.TenantID = "other" }, func(b *RustSnapshotBinding) { b.ProjectID++ }, func(b *RustSnapshotBinding) { b.BasePreparedRequestSHA256 = strings.Repeat("a", 64) }, func(b *RustSnapshotBinding) { b.SourceSHA256 = strings.Repeat("a", 64) }, func(b *RustSnapshotBinding) { b.CargoLockSHA256 = strings.Repeat("a", 64) }, func(b *RustSnapshotBinding) { b.VendorSHA256 = strings.Repeat("a", 64) }, func(b *RustSnapshotBinding) { b.CompilerFlagsSHA256 = strings.Repeat("a", 64) }, func(b *RustSnapshotBinding) { b.ToolchainSHA256 = strings.Repeat("a", 64) }}
	for _, change := range changes {
		c := b
		change(&c)
		changed, err := c.Key()
		if err != nil || changed == key {
			t.Fatal("binding not isolated", err)
		}
	}
}
func TestCompiledSnapshotProfilesAreTrustedAndBounded(t *testing.T) {
	b, _, _ := snapshotFixture(t)
	profiles, err := NewRustSnapshotProfiles([]RustSnapshotProfile{{Binding: b}})
	if err != nil {
		t.Fatal(err)
	}
	b.TenantID = "other"
	b.ProjectID = 3
	if err := profiles.Validate(b, ""); err != nil {
		t.Fatal(err)
	}
	b.AdapterSHA256 = strings.Repeat("a", 64)
	if profiles.Validate(b, "") == nil {
		t.Fatal("untrusted adapter accepted")
	}
	if _, err := NewRustSnapshotProfiles(make([]RustSnapshotProfile, 65)); err == nil {
		t.Fatal("unbounded profiles")
	}
}
func TestCompiledSnapshotExactPreparedInputTimeoutBinding(t *testing.T) {
	b, _, _ := snapshotFixture(t)
	source := "fn main() {}"
	raw, _ := SnapshotJSON(map[string]any{"revision": 1, "language": "rust", "source": source, "input": map[string]any{"x": 1}, "image_digest": b.ExecutionImageDigest, "policy_revision": b.PolicyRevision, "timeout_seconds": 10})
	b.SourceSHA256 = SnapshotContentSHA256([]byte(source))
	b.BasePreparedRequestSHA256 = SnapshotHash("elitea.sandbox.prepared-job.v1\x00", raw)
	if _, err := b.MatchPrepared(raw); err != nil {
		t.Fatal(err)
	}
	for _, changed := range [][]byte{[]byte(strings.Replace(string(raw), `"x":1`, `"x":2`, 1)), []byte(strings.Replace(string(raw), `"timeout_seconds":10`, `"timeout_seconds":20`, 1)), append(append([]byte(nil), raw...), 10)} {
		if _, err := b.MatchPrepared(changed); err == nil {
			t.Fatal("changed prepared input accepted")
		}
	}
}
func TestCompiledSnapshotCompileExecuteIntentDomains(t *testing.T) {
	b, _, root := snapshotFixture(t)
	compile, err := SnapshotJobDigest("compile", b, "")
	if err != nil {
		t.Fatal(err)
	}
	execute, err := SnapshotJobDigest("execute", b, root)
	if err != nil || compile == execute || compile == b.BasePreparedRequestSHA256 {
		t.Fatal("role digest collision")
	}
	if _, err := SnapshotJobDigest("compile", b, root); err == nil {
		t.Fatal("compile root accepted")
	}
	if _, err := SnapshotJobDigest("execute", b, ""); err == nil {
		t.Fatal("execute without pin")
	}
}
func TestCompiledSnapshotReceiptRequiresSuccessAndExactCapturedOutput(t *testing.T) {
	_, raw, root := snapshotFixture(t)
	d, _ := ParseRustSnapshotDescriptor(raw, root)
	stdout, _ := SnapshotJSON(struct {
		Revision int                    `json:"revision"`
		Artifact RustSnapshotDescriptor `json:"compiled_artifact"`
	}{1, d})
	success, _ := SnapshotJSON(map[string]any{"revision": 1, "status": "completed", "exit_code": 0, "stdout": string(stdout) + "\n", "stderr": ""})
	if err := SuccessfulSnapshotReceipt(success, raw); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{[]byte(strings.Replace(string(success), `"exit_code":0`, `"exit_code":1`, 1)), []byte(strings.Replace(string(success), `"completed"`, `"cancelled"`, 1)), []byte(strings.Replace(string(success), `"revision":1`, `"revision":1,"revision":1`, 1))} {
		if SuccessfulSnapshotReceipt(bad, raw) == nil {
			t.Fatal("bad receipt accepted")
		}
	}
	wrong := append([]byte(nil), raw...)
	wrong[10] = '9'
	if SuccessfulSnapshotReceipt(success, wrong) == nil {
		t.Fatal("substitute captured bytes accepted")
	}
	var envelope map[string]any
	_ = json.Unmarshal(success, &envelope)
	delete(envelope, "exit_code")
	bad, _ := SnapshotJSON(envelope)
	if SuccessfulSnapshotReceipt(bad, raw) == nil {
		t.Fatal("missing exit code accepted")
	}
}
