package main

// The Entra Agent ID adapter (#1152): the `entra.agentid` operator block,
// read from ELITEA_ENTRA_AGENT_ID_* or from a JSON file, validated at boot.
// Built only for a governed process whose policy revision allows the
// dedicated_agent mode; otherwise nothing is read and nothing is built.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	agentidentitycore "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/agentid"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
)

const (
	entraAgentIDConfigFileEnv         = "ELITEA_ENTRA_AGENT_ID_CONFIG_FILE"
	entraAgentIDTenantEnv             = "ELITEA_ENTRA_AGENT_ID_TENANT_ID"
	entraAgentIDBlueprintEnv          = "ELITEA_ENTRA_AGENT_ID_BLUEPRINT_APP_ID"
	entraAgentIDFederatedTokenFileEnv = "ELITEA_ENTRA_AGENT_ID_FEDERATED_TOKEN_FILE"
	entraAgentIDAuthorityEnv          = "ELITEA_ENTRA_AGENT_ID_AUTHORITY"
	entraAgentIDGraphBaseURLEnv       = "ELITEA_ENTRA_AGENT_ID_GRAPH_BASE_URL"
	entraAgentIDAllowedScopesEnv      = "ELITEA_ENTRA_AGENT_ID_ALLOWED_RESOURCE_SCOPES"
	entraAgentIDSponsorGroupsEnv      = "ELITEA_ENTRA_AGENT_ID_SPONSOR_GROUPS"
	entraAgentIDIdentityLimitEnv      = "ELITEA_ENTRA_AGENT_ID_IDENTITY_LIMIT"
	// Tripwires, not knobs: set, they refuse boot. The governed profile
	// authenticates the blueprint with a federated credential only.
	entraAgentIDClientSecretEnv    = "ELITEA_ENTRA_AGENT_ID_CLIENT_SECRET"
	entraAgentIDCertificateFileEnv = "ELITEA_ENTRA_AGENT_ID_CERTIFICATE_FILE"

	maxEntraAgentIDConfigBytes = 256 * 1024
)

var entraAgentIDEnvBlock = []string{
	entraAgentIDTenantEnv, entraAgentIDBlueprintEnv, entraAgentIDFederatedTokenFileEnv, entraAgentIDAuthorityEnv,
	entraAgentIDGraphBaseURLEnv, entraAgentIDAllowedScopesEnv, entraAgentIDSponsorGroupsEnv,
	entraAgentIDIdentityLimitEnv, entraAgentIDClientSecretEnv, entraAgentIDCertificateFileEnv,
}

// entraAgentIDFile is the JSON file form: {"entra": {"agentid": {...}}}.
type entraAgentIDFile struct {
	Entra struct {
		AgentID *entraAgentIDBlock `json:"agentid"`
	} `json:"entra"`
}

type entraAgentIDBlock struct {
	TenantID              string            `json:"tenant_id"`
	BlueprintAppID        string            `json:"blueprint_app_id"`
	FederatedTokenFile    string            `json:"federated_token_file"`
	Authority             string            `json:"authority"`
	GraphBaseURL          string            `json:"graph_base_url"`
	AllowedResourceScopes []string          `json:"allowed_resource_scopes"`
	SponsorGroups         map[string]string `json:"sponsor_groups"`
	IdentityLimit         int               `json:"identity_limit"`
	DisplayNamePrefix     string            `json:"display_name_prefix"`
	AssertionAudiences    []string          `json:"assertion_audiences"`
	ClientSecret          string            `json:"client_secret"`
	CertificateFile       string            `json:"certificate_file"`
}

// entraAgentIdentityAtBoot builds the Entra Agent ID provider and the T1
// assertion source for #1117 AgentOBO (obo.AgentConfig.Assertions). Both are
// a real nil, never a typed nil, when the flag is off, when no block is
// configured, or when the policy revision does not allow dedicated_agent.
// A configured but invalid block refuses boot: a multi-tenant alias tenant,
// a client secret or a certificate among them.
//
// sessions is the agent session store; delegations is the Entra delegation
// custody, nil when Entra delegation is not configured (every Agent OBO is
// then denied).
func entraAgentIdentityAtBoot(
	admission profile.Admission,
	revision profile.PolicyRevision,
	lookup func(string) (string, bool),
	sessions *repos.AgentSessions,
	delegations *delegationstore.Store,
	logger *slog.Logger,
) (agentidentitycore.Provider, obo.AgentAssertionSource, error) {
	if !admission.Governed() {
		return nil, nil, nil
	}
	block, configured, err := entraAgentIDBlockFrom(lookup)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", profile.ErrBootRefused, err)
	}
	if !configured {
		return nil, nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if !revision.Document.AllowsIdentityMode(identity.IdentityModeDedicatedAgent) {
		logger.Warn("the Entra Agent ID provider is configured but the policy revision does not allow dedicated_agent; it is not built",
			"revision", revision.ID)
		return nil, nil, nil
	}
	if sessions == nil {
		return nil, nil, fmt.Errorf("%w: the Entra Agent ID provider needs the agent session store", profile.ErrBootRefused)
	}
	cfg, err := block.config()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", profile.ErrBootRefused, err)
	}
	cfg.Sessions, cfg.Logger = sessions, logger
	if delegations != nil {
		cfg.Delegations = delegations
	}
	provider, err := agentid.New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", profile.ErrBootRefused, err)
	}
	logger.Info("Entra Agent ID provider enabled", "tenant", provider.TenantID(), "blueprint", provider.BlueprintAppID(),
		"identity_limit", provider.Capabilities().IdentityLimit, "agent_obo", delegations != nil)
	return provider, provider.AssertionSource(), nil
}

// entraAgentIDBlockFrom reads the file or the env block. Both set is
// refused: one source of truth.
func entraAgentIDBlockFrom(lookup func(string) (string, bool)) (entraAgentIDBlock, bool, error) {
	get := func(key string) string {
		value, _ := lookup(key)
		return strings.TrimSpace(value)
	}
	envSet := false
	for _, key := range entraAgentIDEnvBlock {
		if get(key) != "" {
			envSet = true
		}
	}
	path := get(entraAgentIDConfigFileEnv)
	switch {
	case path != "" && envSet:
		return entraAgentIDBlock{}, false, fmt.Errorf("set either %s or the %s* variables, not both", entraAgentIDConfigFileEnv, "ELITEA_ENTRA_AGENT_ID_")
	case path != "":
		block, err := readEntraAgentIDFile(path)
		return block, err == nil, err
	case !envSet:
		return entraAgentIDBlock{}, false, nil
	}
	block := entraAgentIDBlock{
		TenantID: get(entraAgentIDTenantEnv), BlueprintAppID: get(entraAgentIDBlueprintEnv),
		FederatedTokenFile: get(entraAgentIDFederatedTokenFileEnv), Authority: get(entraAgentIDAuthorityEnv),
		GraphBaseURL: get(entraAgentIDGraphBaseURLEnv), ClientSecret: get(entraAgentIDClientSecretEnv),
		CertificateFile:       get(entraAgentIDCertificateFileEnv),
		AllowedResourceScopes: strings.FieldsFunc(get(entraAgentIDAllowedScopesEnv), func(r rune) bool { return r == ',' || r == ' ' }),
	}
	if raw := get(entraAgentIDIdentityLimitEnv); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			return entraAgentIDBlock{}, false, fmt.Errorf("%s must be an integer", entraAgentIDIdentityLimitEnv)
		}
		block.IdentityLimit = limit
	}
	if raw := get(entraAgentIDSponsorGroupsEnv); raw != "" {
		block.SponsorGroups = map[string]string{}
		for _, pair := range strings.Split(raw, ",") {
			key, group, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if !ok || key == "" || group == "" {
				return entraAgentIDBlock{}, false, fmt.Errorf("%s takes default=<group id>,<project id>=<group id>", entraAgentIDSponsorGroupsEnv)
			}
			block.SponsorGroups[key] = group
		}
	}
	return block, true, nil
}

func readEntraAgentIDFile(path string) (entraAgentIDBlock, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return entraAgentIDBlock{}, fmt.Errorf("read %s failed", entraAgentIDConfigFileEnv)
	}
	if len(raw) > maxEntraAgentIDConfigBytes {
		return entraAgentIDBlock{}, fmt.Errorf("%s exceeds %d bytes", entraAgentIDConfigFileEnv, maxEntraAgentIDConfigBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var file entraAgentIDFile
	if err := decoder.Decode(&file); err != nil {
		// The decoder error names the field, never a value.
		return entraAgentIDBlock{}, fmt.Errorf("%s is not a valid entra.agentid document: %w", entraAgentIDConfigFileEnv, err)
	}
	if file.Entra.AgentID == nil {
		return entraAgentIDBlock{}, errors.New(entraAgentIDConfigFileEnv + " has no entra.agentid block")
	}
	return *file.Entra.AgentID, nil
}

func (b entraAgentIDBlock) config() (agentid.Config, error) {
	cfg := agentid.Config{
		TenantID: b.TenantID, BlueprintAppID: b.BlueprintAppID, FederatedTokenFile: b.FederatedTokenFile,
		Authority: b.Authority, GraphBaseURL: b.GraphBaseURL, AllowedResourceScopes: b.AllowedResourceScopes,
		IdentityLimit: b.IdentityLimit, DisplayNamePrefix: b.DisplayNamePrefix, AssertionAudiences: b.AssertionAudiences,
		ClientSecret: b.ClientSecret, CertificateFile: b.CertificateFile,
	}
	for key, group := range b.SponsorGroups {
		if key == "default" {
			cfg.DefaultSponsorGroupID = group
			continue
		}
		project, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			return agentid.Config{}, fmt.Errorf("sponsor group key %q must be \"default\" or a project id", key)
		}
		if cfg.SponsorGroups == nil {
			cfg.SponsorGroups = map[int64]string{}
		}
		cfg.SponsorGroups[project] = group
	}
	return cfg, nil
}
