package runtimecomposition

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

const compiledSnapshotConfigPrefix = "ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_"
const CompiledSnapshotAgentStateDSNFileEnv = "ELITEA_RUST_COMPILED_AGENTSTATE_DSN_FILE"
const compiledSnapshotProfilesLimit = 1024 * 1024
const compiledSnapshotRequestLimit = 1024*1024 + 80*1024
const CompiledSnapshotAgentStateMaxConns int32 = 4

// CompiledSnapshotConfig is operator startup authority. A caller cannot replace
// its pinned profile material, quotas or separate original-receipt database.
type CompiledSnapshotConfig struct {
	ProfilesFile      string
	ProfilesSHA256    string
	AgentStateDSNFile string
	Quota             repos.SnapshotQuota
}

var compiledSnapshotSettingNames = []string{"PROFILES_FILE", "PROFILES_SHA256", "GLOBAL_ENTRIES", "GLOBAL_BYTES", "TENANT_ENTRIES", "TENANT_BYTES", "PUBLISHING_TTL_SECONDS", "READY_TTL_SECONDS"}

func compiledSnapshotSettingsPresent(lookup LookupEnv) bool {
	for _, suffix := range compiledSnapshotSettingNames {
		if value, _ := lookup(compiledSnapshotConfigPrefix + suffix); value != "" {
			return true
		}
	}
	value, _ := lookup(CompiledSnapshotAgentStateDSNFileEnv)
	return value != ""
}
func compiledSnapshotConfigFromEnv(lookup LookupEnv, enabled, agent bool, audiences []string) (*CompiledSnapshotConfig, error) {
	flag, _ := lookup(compiledSnapshotConfigPrefix + "ENABLED")
	if flag == "" || flag == "false" {
		if compiledSnapshotSettingsPresent(lookup) {
			return nil, errors.New("compiled snapshot settings require explicit enablement")
		}
		return nil, nil
	}
	if flag != "true" {
		return nil, errors.New("compiled snapshot enablement must be true or false")
	}
	if !enabled || !agent || len(audiences) == 0 {
		return nil, errors.New("compiled snapshots require active agent dispatch and exact supervisor audiences")
	}
	required := func(name string) (string, error) {
		value, _ := lookup(name)
		if value == "" {
			return "", fmt.Errorf("%s is required for compiled snapshots", name)
		}
		return value, nil
	}
	var c CompiledSnapshotConfig
	var err error
	if c.ProfilesFile, err = required(compiledSnapshotConfigPrefix + "PROFILES_FILE"); err != nil {
		return nil, err
	}
	if c.ProfilesSHA256, err = required(compiledSnapshotConfigPrefix + "PROFILES_SHA256"); err != nil {
		return nil, err
	}
	if c.AgentStateDSNFile, err = required(CompiledSnapshotAgentStateDSNFileEnv); err != nil {
		return nil, err
	}
	integer := func(suffix string) (int64, error) {
		name := compiledSnapshotConfigPrefix + suffix
		raw, err := required(name)
		if err != nil {
			return 0, err
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 || strconv.FormatInt(value, 10) != raw {
			return 0, fmt.Errorf("%s must be a canonical positive integer", name)
		}
		return value, nil
	}
	if c.Quota.GlobalEntries, err = integer("GLOBAL_ENTRIES"); err != nil {
		return nil, err
	}
	if c.Quota.GlobalBytes, err = integer("GLOBAL_BYTES"); err != nil {
		return nil, err
	}
	if c.Quota.TenantEntries, err = integer("TENANT_ENTRIES"); err != nil {
		return nil, err
	}
	if c.Quota.TenantBytes, err = integer("TENANT_BYTES"); err != nil {
		return nil, err
	}
	stage, err := integer("PUBLISHING_TTL_SECONDS")
	if err != nil || stage > 300 {
		return nil, errors.New("compiled snapshot publishing TTL must be 1..300 seconds")
	}
	ready, err := integer("READY_TTL_SECONDS")
	if err != nil || ready > 86400 {
		return nil, errors.New("compiled snapshot ready TTL must be 1..86400 seconds")
	}
	c.Quota.PublishingTTL = time.Duration(stage) * time.Second
	c.Quota.ReadyTTL = time.Duration(ready) * time.Second
	if err = c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
func (c CompiledSnapshotConfig) Validate() error {
	if !validPrivateConfigPath(c.ProfilesFile) || !validPrivateConfigPath(c.AgentStateDSNFile) || c.ProfilesFile == c.AgentStateDSNFile || !domain.SnapshotDigest(c.ProfilesSHA256) || c.Quota.Validate() != nil {
		return errors.New("compiled snapshot operator material or quota configuration is invalid")
	}
	return nil
}

type compiledProfileRecord struct {
	Binding                domain.RustSnapshotBinding `json:"binding"`
	DependencyBundleSHA256 string                     `json:"dependency_bundle_sha256"`
}
type compiledProfileManifest struct {
	Revision uint32                  `json:"revision"`
	Profiles []compiledProfileRecord `json:"profiles"`
}

func parseCompiledSnapshotProfiles(raw []byte, pin string) (*domain.RustSnapshotProfiles, error) {
	if len(raw) < 1 || len(raw) > compiledSnapshotProfilesLimit || !domain.SnapshotDigest(pin) || domain.SnapshotContentSHA256(raw) != pin {
		return nil, errors.New("compiled snapshot profile material does not match its immutable pin")
	}
	var manifest compiledProfileManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&manifest) != nil || decoder.Decode(new(any)) != io.EOF || manifest.Revision != 1 || len(manifest.Profiles) < 1 || len(manifest.Profiles) > 64 {
		return nil, errors.New("compiled snapshot profile manifest is malformed")
	}
	canonical, err := domain.SnapshotJSON(manifest)
	if err != nil || !bytes.Equal(canonical, raw) {
		return nil, errors.New("compiled snapshot profile manifest must have exact canonical fields")
	}
	profiles := make([]domain.RustSnapshotProfile, 0, len(manifest.Profiles))
	seen := map[string]struct{}{}
	for _, p := range manifest.Profiles {
		if p.Binding.Validate() != nil || p.DependencyBundleSHA256 != "" && !domain.SnapshotDigest(p.DependencyBundleSHA256) {
			return nil, errors.New("compiled snapshot profile is invalid")
		}
		key, _ := p.Binding.Key()
		identity := key + ":" + p.DependencyBundleSHA256
		if _, ok := seen[identity]; ok {
			return nil, errors.New("compiled snapshot release profiles are duplicated")
		}
		seen[identity] = struct{}{}
		profiles = append(profiles, domain.RustSnapshotProfile{Binding: p.Binding, DependencyBundleSHA256: p.DependencyBundleSHA256})
	}
	return domain.NewRustSnapshotProfiles(profiles)
}
func loadCompiledSnapshotProfiles(c CompiledSnapshotConfig) (*domain.RustSnapshotProfiles, error) {
	if c.Validate() != nil {
		return nil, errors.New("compiled snapshot operator configuration is invalid")
	}
	raw, err := securefile.Read(c.ProfilesFile, compiledSnapshotProfilesLimit, securefile.PublicMaterial)
	if err != nil {
		return nil, fmt.Errorf("read compiled snapshot release profiles: %w", err)
	}
	return parseCompiledSnapshotProfiles(raw, c.ProfilesSHA256)
}
