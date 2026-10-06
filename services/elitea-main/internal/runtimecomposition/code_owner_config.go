package runtimecomposition

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/url"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Nil is the default. Endpoints contain no credential or worker-provided URL.
type CodeOwnerConfig struct {
	MainWorkloadIdentity string                `json:"main_workload_identity"`
	CertificateChainPath string                `json:"certificate_chain_path"`
	PrivateKeyPath       string                `json:"private_key_path"`
	ServerCAPath         string                `json:"server_ca_path"`
	Supervisors          []CodeOwnerSupervisor `json:"supervisors"`
}
type CodeOwnerSupervisor struct {
	Audience    string `json:"audience"`
	HTTPSOrigin string `json:"https_origin"`
}

func codeOwnerConfigFromEnv(lookup LookupEnv, enabled, agents bool, audiences []string) (*CodeOwnerConfig, error) {
	flag, _ := lookup("ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED")
	if flag == "" || flag == "false" {
		if raw, _ := lookup("ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG"); raw != "" {
			return nil, errors.New("code owner recovery settings require explicit enablement")
		}
		return nil, nil
	}
	if flag != "true" || !enabled || !agents {
		return nil, errors.New("code owner recovery requires native Agent dispatch")
	}
	raw, _ := lookup("ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG")
	if len(raw) == 0 || len(raw) > 16384 {
		return nil, errors.New("code owner recovery configuration is required")
	}
	var cfg CodeOwnerConfig
	if code.Decode([]byte(raw), &cfg, 16384) != nil || !codeOwnerConfigFields([]byte(raw)) {
		return nil, errors.New("code owner recovery configuration is invalid")
	}
	if err := validateCodeOwnerConfig(&cfg, enabled, agents, audiences); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func codeOwnerConfigFields(raw []byte) bool {
	if !code.RequiredFields(raw, []string{"main_workload_identity", "certificate_chain_path", "private_key_path", "server_ca_path", "supervisors"}) {
		return false
	}
	var fields map[string]json.RawMessage
	var supervisors []json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(fields["supervisors"], &supervisors) != nil {
		return false
	}
	for _, supervisor := range supervisors {
		if !code.RequiredFields(supervisor, []string{"audience", "https_origin"}) {
			return false
		}
	}
	return true
}

func configureCodeOwner(config Config, pool *pgxpool.Pool, key ed25519.PrivateKey, index runtime.RustSnapshotIndex, profiles *runtime.RustSnapshotProfiles, sources repos.CodeDefinitionSourceReader) (*repos.CodeIntentRepository, *storage.CodeOwnerClient, *storage.CodeOwnerGrantSigner, error) {
	if config.CodeOwnerRecovery == nil {
		return nil, nil, nil, nil
	}
	cfg := config.CodeOwnerRecovery
	certificateBytes, err := securefile.Read(cfg.CertificateChainPath, 1<<20, securefile.PublicMaterial)
	if err != nil {
		return nil, nil, nil, errors.New("code owner client TLS identity unavailable")
	}
	keyBytes, err := securefile.Read(cfg.PrivateKeyPath, 1<<20, securefile.PrivateMaterial)
	if err != nil {
		return nil, nil, nil, errors.New("code owner client TLS identity unavailable")
	}
	defer clear(keyBytes)
	certificate, err := tls.X509KeyPair(certificateBytes, keyBytes)
	if err != nil {
		return nil, nil, nil, errors.New("code owner client TLS identity unavailable")
	}
	ca, err := securefile.Read(cfg.ServerCAPath, 1<<20, securefile.PublicMaterial)
	if err != nil {
		return nil, nil, nil, errors.New("code owner server CA unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, nil, nil, errors.New("code owner server CA invalid")
	}
	endpoints := make([]storage.CodeOwnerEndpoint, 0, len(cfg.Supervisors))
	audiences := make([]string, 0, len(cfg.Supervisors))
	for _, s := range cfg.Supervisors {
		endpoints = append(endpoints, storage.CodeOwnerEndpoint{Audience: s.Audience, Origin: s.HTTPSOrigin, TLS: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}}})
		audiences = append(audiences, s.Audience)
	}
	client, err := storage.NewCodeOwnerClient(cfg.MainWorkloadIdentity, endpoints)
	if err != nil {
		return nil, nil, nil, err
	}
	signer, err := storage.NewCodeOwnerGrantSigner(config.SigningKeyID, key, cfg.MainWorkloadIdentity, audiences)
	if err != nil {
		client.Close()
		return nil, nil, nil, err
	}
	repo, err := repos.NewCodeIntentRepository(pool, signer, client, sources, index, profiles)
	if err != nil {
		client.Close()
		return nil, nil, nil, err
	}
	return repo, client, signer, nil
}

func validateCodeOwnerConfig(cfg *CodeOwnerConfig, enabled, agents bool, audiences []string) error {
	if cfg == nil {
		return nil
	}
	if !enabled || !agents || !code.Identity(cfg.MainWorkloadIdentity) || len(cfg.Supervisors) == 0 || len(cfg.Supervisors) > 16 {
		return errors.New("code owner recovery requires active Agent dispatch and exact identities")
	}
	for _, path := range []string{cfg.CertificateChainPath, cfg.PrivateKeyPath, cfg.ServerCAPath} {
		if !validPrivateConfigPath(path) {
			return errors.New("code owner recovery TLS paths are invalid")
		}
	}
	allowed := map[string]bool{}
	for _, audience := range audiences {
		allowed[audience] = true
	}
	seen := map[string]bool{}
	for _, supervisor := range cfg.Supervisors {
		u, err := url.Parse(supervisor.HTTPSOrigin)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.Path != "" && u.Path != "/" || !code.Identity(supervisor.Audience) || !allowed[supervisor.Audience] || seen[supervisor.Audience] {
			return errors.New("code owner Supervisor must match configured sandbox audience and fixed HTTPS origin")
		}
		seen[supervisor.Audience] = true
	}
	return nil
}
