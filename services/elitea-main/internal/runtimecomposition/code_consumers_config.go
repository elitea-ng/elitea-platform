package runtimecomposition

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

const codeConsumerConfigLimit = 16 * 1024
const CodeDebugAgentStateDSNFileEnv = "ELITEA_CODE_DEBUG_AGENTSTATE_DSN_FILE"
const CodeDebugAgentStateMaxConns int32 = 4

// CodeWorkspaceConfig selects existing repository readers and their frozen limits.
// It contains no credential or authored repository selection.
type CodeWorkspaceConfig struct {
	Revision               uint32                      `json:"revision"`
	RepositoryCapabilities []string                    `json:"repository_capabilities"`
	EgressAllowlist        []string                    `json:"egress_allowlist"`
	Policy                 storage.CodeWorkspacePolicy `json:"policy"`
}

// CodePlatformDeploymentConfig holds bounded policies and a private content key file.
// Native actor, project, toolkit, and credential permissions remain authoritative.
type CodePlatformDeploymentConfig struct {
	Revision        uint32                     `json:"revision"`
	ContentKeysFile string                     `json:"content_keys_file"`
	BrokerPolicies  []storage.CodeBrokerPolicy `json:"broker_policies"`
}

type CodeDebugArtifactsConfig struct {
	AgentStateDSNFile string
}

func codeConsumerFlag(lookup LookupEnv, feature, materialName string) (bool, error) {
	flag, _ := lookup("ELITEA_RUNTIME_CODE_" + feature + "_ENABLED")
	if flag == "" || flag == "false" {
		if raw, _ := lookup(materialName); raw != "" {
			return false, errors.New("code consumer settings require explicit enablement")
		}
		return false, nil
	}
	if flag != "true" {
		return false, errors.New("code consumer enablement must be true or false")
	}
	return true, nil
}

func codeConsumerConfigFromEnv(lookup LookupEnv, owner *CodeOwnerConfig) (*CodeWorkspaceConfig, *CodePlatformDeploymentConfig, *CodeDebugArtifactsConfig, error) {
	workspaceEnabled, err := codeConsumerFlag(lookup, "WORKSPACE", "ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG")
	if err != nil {
		return nil, nil, nil, err
	}
	platformEnabled, err := codeConsumerFlag(lookup, "PLATFORM", "ELITEA_RUNTIME_CODE_PLATFORM_CONFIG")
	if err != nil {
		return nil, nil, nil, err
	}
	debugEnabled, err := codeConsumerFlag(lookup, "DEBUG_ARTIFACTS", CodeDebugAgentStateDSNFileEnv)
	if err != nil {
		return nil, nil, nil, err
	}
	if (workspaceEnabled || platformEnabled || debugEnabled) && owner == nil {
		return nil, nil, nil, errors.New("code consumers require original Code owner composition")
	}
	var workspace *CodeWorkspaceConfig
	var platform *CodePlatformDeploymentConfig
	var debug *CodeDebugArtifactsConfig
	if workspaceEnabled {
		raw, _ := lookup("ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG")
		workspace = new(CodeWorkspaceConfig)
		if code.Decode([]byte(raw), workspace, codeConsumerConfigLimit) != nil ||
			!codeWorkspaceConfigFields([]byte(raw)) || workspace.Validate() != nil {
			return nil, nil, nil, errors.New("code workspace operator configuration is invalid")
		}
	}
	if platformEnabled {
		raw, _ := lookup("ELITEA_RUNTIME_CODE_PLATFORM_CONFIG")
		platform = new(CodePlatformDeploymentConfig)
		if code.Decode([]byte(raw), platform, codeConsumerConfigLimit) != nil ||
			!codePlatformConfigFields([]byte(raw)) || platform.Validate() != nil {
			return nil, nil, nil, errors.New("code platform operator configuration is invalid")
		}
	}
	if debugEnabled {
		path, _ := lookup(CodeDebugAgentStateDSNFileEnv)
		debug = &CodeDebugArtifactsConfig{AgentStateDSNFile: path}
		if debug.Validate() != nil {
			return nil, nil, nil, errors.New("code debug AgentState material path is invalid")
		}
	}
	return workspace, platform, debug, nil
}

func codeWorkspaceConfigFields(raw []byte) bool {
	if !code.RequiredFields(raw, []string{"revision", "repository_capabilities", "egress_allowlist", "policy"}) {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	return code.RequiredFields(fields["policy"], []string{"revision", "max_files", "max_file_bytes", "max_total_bytes", "max_manifest_bytes", "max_path_bytes", "max_depth", "max_projections", "max_acquisition_seconds"})
}

func codePlatformConfigFields(raw []byte) bool {
	if !code.RequiredFields(raw, []string{"revision", "content_keys_file", "broker_policies"}) {
		return false
	}
	var fields map[string]json.RawMessage
	var policies []json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(fields["broker_policies"], &policies) != nil {
		return false
	}
	for _, policy := range policies {
		if !code.RequiredFields(policy, []string{"revision", "max_calls", "max_total_bytes"}) {
			return false
		}
	}
	return true
}

func (c CodeWorkspaceConfig) Validate() error {
	if c.Revision != 1 || len(c.RepositoryCapabilities) != 1 || c.RepositoryCapabilities[0] != "github" || c.Policy.Validate() != nil {
		return errors.New("code workspace capability or policy is invalid")
	}
	_, err := c.egressPolicy()
	return err
}

func (c CodeWorkspaceConfig) egressPolicy() (*egresslib.Allowlist, error) {
	if len(c.EgressAllowlist) < 1 || len(c.EgressAllowlist) > 16 {
		return nil, errors.New("code workspace requires bounded exact egress entries")
	}
	seen := make(map[string]bool, len(c.EgressAllowlist))
	for _, raw := range c.EgressAllowlist {
		entry, err := egresslib.ParseEntry(raw)
		host, port, splitErr := net.SplitHostPort(raw)
		origin, urlErr := url.Parse("https://" + raw)
		number, portErr := strconv.ParseUint(port, 10, 16)
		if err != nil || entry.Kind() != egresslib.KindExact || entry.String() != raw || len(raw) > 320 || seen[raw] ||
			splitErr != nil || portErr != nil || number == 0 || strconv.FormatUint(number, 10) != port ||
			urlErr != nil || origin.Host != raw || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" ||
			!codeRepositoryHost(host) {
			return nil, errors.New("code workspace egress requires canonical exact host and port entries")
		}
		seen[raw] = true
	}
	return egresslib.Parse(c.EgressAllowlist)
}

func codeRepositoryHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
	}
	if host == "" || host == "localhost" || len(host) > 253 || strings.ToLower(host) != host {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
				return false
			}
		}
	}
	return true
}

// OpenCapabilities creates Main-owned readers. Close the returned owner at shutdown.
func (c CodeWorkspaceConfig) OpenCapabilities() (*storage.CodeRepositoryCapabilities, func(), error) {
	if err := c.Validate(); err != nil {
		return nil, nil, err
	}
	allowed, err := c.egressPolicy()
	if err != nil {
		return nil, nil, err
	}
	github, err := storage.NewCodeWorkspaceGitHub(allowed)
	if err != nil {
		return nil, nil, err
	}
	capabilities, err := storage.NewCodeRepositoryCapabilities(map[string]storage.CodeRepositoryCapability{"github": github})
	if err != nil {
		github.Close()
		return nil, nil, err
	}
	return capabilities, github.Close, nil
}

func (c CodePlatformDeploymentConfig) Validate() error {
	if c.Revision != 1 || !validPrivateConfigPath(c.ContentKeysFile) {
		return errors.New("code platform revision or material path is invalid")
	}
	_, err := storage.NewCodeBrokerPolicies(c.BrokerPolicies)
	return err
}

func (c CodeDebugArtifactsConfig) Validate() error {
	if !validPrivateConfigPath(c.AgentStateDSNFile) {
		return errors.New("code debug AgentState material path is invalid")
	}
	return nil
}

func validateCodeConsumerConfig(c Config) error {
	if c.CodeWorkspace == nil && c.CodePlatform == nil && c.CodeDebugArtifacts == nil {
		return nil
	}
	if c.CodeOwnerRecovery == nil || !c.Enabled || !c.AgentExecutionDispatchEnabled {
		return errors.New("code consumers require active Agent dispatch and original Code owner composition")
	}
	if c.CodeWorkspace != nil {
		if err := c.CodeWorkspace.Validate(); err != nil {
			return err
		}
	}
	if c.CodePlatform != nil {
		if err := c.CodePlatform.Validate(); err != nil {
			return err
		}
		// Private encryption keys cannot share a named file with other runtime material.
		paths := []string{c.SigningKeyFile, c.VerificationKeyringFile, c.RedisPasswordFile, c.RedisCAFile,
			c.ControlTLS.CertificateChainPath, c.ControlTLS.PrivateKeyPath, c.ControlTLS.ClientCAPath,
			c.OutputTLS.CertificateChainPath, c.OutputTLS.PrivateKeyPath, c.OutputTLS.ClientCAPath,
			c.ContentTLS.CertificateChainPath, c.ContentTLS.PrivateKeyPath, c.ContentTLS.ClientCAPath,
			c.CodeOwnerRecovery.CertificateChainPath, c.CodeOwnerRecovery.PrivateKeyPath, c.CodeOwnerRecovery.ServerCAPath}
		if c.RustCompiledSnapshots != nil {
			paths = append(paths, c.RustCompiledSnapshots.ProfilesFile, c.RustCompiledSnapshots.AgentStateDSNFile)
		}
		if c.CodeDebugArtifacts != nil {
			paths = append(paths, c.CodeDebugArtifacts.AgentStateDSNFile)
		}
		for _, path := range paths {
			if c.CodePlatform.ContentKeysFile == path {
				return errors.New("code platform content keys require separate private material")
			}
		}
	}
	if c.CodeDebugArtifacts != nil {
		return c.CodeDebugArtifacts.Validate()
	}
	return nil
}
