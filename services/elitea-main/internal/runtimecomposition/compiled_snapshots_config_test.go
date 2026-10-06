package runtimecomposition

import (
	"bytes"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func compiledTestConfig() CompiledSnapshotConfig {
	return CompiledSnapshotConfig{ProfilesFile: "/run/elitea-runtime/rust-compiled-profiles.json", ProfilesSHA256: strings.Repeat("a", 64), AgentStateDSNFile: "/run/elitea-runtime/agentstate-dsn", Quota: repos.SnapshotQuota{GlobalEntries: 100, GlobalBytes: 1 << 30, TenantEntries: 10, TenantBytes: 1 << 28, PublishingTTL: time.Minute, ReadyTTL: time.Hour}}
}
func compiledTestEnvironment() map[string]string {
	c := compiledTestConfig()
	return map[string]string{compiledSnapshotConfigPrefix + "ENABLED": "true", compiledSnapshotConfigPrefix + "PROFILES_FILE": c.ProfilesFile, compiledSnapshotConfigPrefix + "PROFILES_SHA256": c.ProfilesSHA256, CompiledSnapshotAgentStateDSNFileEnv: c.AgentStateDSNFile, compiledSnapshotConfigPrefix + "GLOBAL_ENTRIES": "100", compiledSnapshotConfigPrefix + "GLOBAL_BYTES": "1073741824", compiledSnapshotConfigPrefix + "TENANT_ENTRIES": "10", compiledSnapshotConfigPrefix + "TENANT_BYTES": "268435456", compiledSnapshotConfigPrefix + "PUBLISHING_TTL_SECONDS": "60", compiledSnapshotConfigPrefix + "READY_TTL_SECONDS": "3600"}
}
func compiledTestProfile(t *testing.T) ([]byte, domain.RustSnapshotBinding) {
	t.Helper()
	raw, err := os.ReadFile("../domain/runtime/testdata/compiled-snapshot-v1/binding.json")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := domain.ParseRustSnapshotBinding(raw)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := domain.SnapshotJSON(compiledProfileManifest{Revision: 1, Profiles: []compiledProfileRecord{{Binding: binding}}})
	if err != nil {
		t.Fatal(err)
	}
	return manifest, binding
}
func TestCompiledSnapshotConfigExplicitBoundedAuthority(t *testing.T) {
	if c, err := compiledSnapshotConfigFromEnv(mapLookup(nil), false, false, nil); err != nil || c != nil {
		t.Fatal(c, err)
	}
	good := compiledTestEnvironment()
	c, err := compiledSnapshotConfigFromEnv(mapLookup(good), true, true, []string{"supervisor"})
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	cases := []struct{ name, key, value string }{{"not-enabled", "ENABLED", "false"}, {"nonboolean", "ENABLED", "yes"}, {"no-profiles", "PROFILES_FILE", ""}, {"relative-path", "PROFILES_FILE", "profiles.json"}, {"unclean-path", "PROFILES_FILE", "/run/../run/profiles.json"}, {"bad-pin", "PROFILES_SHA256", strings.Repeat("A", 64)}, {"unbounded-count", "GLOBAL_ENTRIES", "100001"}, {"unbounded-bytes", "GLOBAL_BYTES", "1099511627777"}, {"signed-integer", "TENANT_ENTRIES", "+10"}, {"leading-zero", "TENANT_ENTRIES", "010"}, {"stage-ttl", "PUBLISHING_TTL_SECONDS", "301"}, {"ready-ttl", "READY_TTL_SECONDS", "86401"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := compiledTestEnvironment()
			env[compiledSnapshotConfigPrefix+tc.key] = tc.value
			if _, err := compiledSnapshotConfigFromEnv(mapLookup(env), true, true, []string{"supervisor"}); err == nil {
				t.Fatal("invalid operator setting accepted")
			}
		})
	}
	for _, tc := range []struct {
		enabled, agent bool
		audiences      []string
	}{{false, true, []string{"s"}}, {true, false, []string{"s"}}, {true, true, nil}} {
		if _, err := compiledSnapshotConfigFromEnv(mapLookup(good), tc.enabled, tc.agent, tc.audiences); err == nil {
			t.Fatal("missing runtime authority accepted")
		}
	}
	delete(good, CompiledSnapshotAgentStateDSNFileEnv)
	if _, err := compiledSnapshotConfigFromEnv(mapLookup(good), true, true, []string{"s"}); err == nil {
		t.Fatal("DSN-file omission accepted")
	}
}
func TestCompiledSnapshotProfilesImmutableExactFiniteRelease(t *testing.T) {
	raw, b := compiledTestProfile(t)
	pin := domain.SnapshotContentSHA256(raw)
	profiles, err := parseCompiledSnapshotProfiles(raw, pin)
	if err != nil {
		t.Fatal(err)
	}
	b.TenantID = "different"
	b.ProjectID = 42
	b.SourceSHA256 = strings.Repeat("b", 64)
	b.BasePreparedRequestSHA256 = strings.Repeat("c", 64)
	if err := profiles.Validate(b, ""); err != nil {
		t.Fatal("request-scoped fields cannot vary", err)
	}
	b.ToolchainSHA256 = strings.Repeat("d", 64)
	if profiles.Validate(b, "") == nil {
		t.Fatal("caller attested untrusted toolchain")
	}
	bads := [][]byte{append(append([]byte(nil), raw...), 10), []byte(strings.Replace(string(raw), `"revision":1`, `"revision":1,"revision":1`, 1)), []byte(strings.Replace(string(raw), `"revision":1`, `"revision":1,"unknown":0`, 1)), bytes.Repeat([]byte("x"), compiledSnapshotProfilesLimit+1)}
	for _, bad := range bads {
		if _, err := parseCompiledSnapshotProfiles(bad, domain.SnapshotContentSHA256(bad)); err == nil {
			t.Fatal("noncanonical material accepted")
		}
	}
	if _, err := parseCompiledSnapshotProfiles(raw, strings.Repeat("a", 64)); err == nil {
		t.Fatal("changed profile pin accepted")
	}
	_, original := compiledTestProfile(t)
	for _, list := range [][]compiledProfileRecord{nil, {{Binding: original}, {Binding: original}}, make([]compiledProfileRecord, 65)} {
		data, _ := domain.SnapshotJSON(compiledProfileManifest{1, list})
		if _, err := parseCompiledSnapshotProfiles(data, domain.SnapshotContentSHA256(data)); err == nil {
			t.Fatal("empty/duplicate/unbounded profiles accepted")
		}
	}
}
func TestCompiledSnapshotProfileFileUsesSecureBoundedReader(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := compiledTestProfile(t)
	c := compiledTestConfig()
	c.ProfilesFile = filepath.Join(root, "profiles.json")
	c.AgentStateDSNFile = filepath.Join(root, "dsn")
	c.ProfilesSHA256 = domain.SnapshotContentSHA256(raw)
	if err := os.WriteFile(c.ProfilesFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCompiledSnapshotProfiles(c); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "profile-link")
	if err := os.Symlink(c.ProfilesFile, link); err != nil {
		t.Fatal(err)
	}
	linked := c
	linked.ProfilesFile = link
	if _, err := loadCompiledSnapshotProfiles(linked); err == nil {
		t.Fatal("symlink profile accepted")
	}
	if err := os.Chmod(c.ProfilesFile, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCompiledSnapshotProfiles(c); err == nil {
		t.Fatal("mutable public profile accepted")
	}
}
func TestCompiledSnapshotMaterialInventoryIncludesOriginalAgentState(t *testing.T) {
	c, err := ConfigFromEnv(mapLookup(oneDirectoryEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	c.AgentExecutionDispatchEnabled = true
	c.CurrentMainBaseURL = "http://127.0.0.1:8080"
	c.AgentExecutionCommandStream = "ELITEA_RT_V1_AGENT"
	c.SandboxAudiences = []string{"elitea.supervisor"}
	cfg := compiledTestConfig()
	c.RustCompiledSnapshots = &cfg
	files, err := c.MaterialFiles()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]securefile.Permissions{}
	for _, f := range files {
		found[f.Path] = f.Permissions
	}
	if found[cfg.ProfilesFile] != securefile.PublicMaterial || found[cfg.AgentStateDSNFile] != securefile.PrivateMaterial {
		t.Fatal("operator materials not included with owning permission")
	}
	if _, err := c.MaterialDirectory(); err != nil {
		t.Fatal(err)
	}
}
