package main

// The Elitea-native agent identity provider (ADR-0039 decision 12, #1153):
// its relying parties, its dedicated key ring and its public documents.
//
// Built only for a governed process whose active revision allows
// `elitea_native` and whose ELITEA_AGENT_ASSERTION_CONFIG names at least one
// relying party. Otherwise nothing is read beyond that variable, no key is
// loaded, and the JWKS and issuer metadata routes are not mounted (404).

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib/runtoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egresstoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/publicorigin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore/backends"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/security/securefile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// eliteaNativeAgentIdentity is the provider and its public documents. Both
// are nil when the provider is not configured.
type eliteaNativeAgentIdentity struct {
	// Provider joins the agent identity registry. Only the session broker
	// reaches it, and only it signs.
	Provider *eliteanative.Provider
	// WellKnown serves the JWKS and the issuer metadata (public keys only).
	WellKnown *eliteanative.WellKnown
}

// The key ring's reads through the SecretStore seam carry this context. The
// workload scope selects the Vault role ("<ROLE_PREFIX>-agent-assertion").
const (
	agentAssertionWorkloadScope = "agent-assertion"
	agentAssertionCredentialRef = "platform.agent-assertion-key-ring"
	agentAssertionDecision      = "boot.agent-assertion-key-ring"
	maxForeignKeyFileBytes      = 1 << 20
)

// eliteaNativeAgentIdentityAtBoot builds the Elitea-native provider. A
// configuration that is set but invalid, a key that cannot be loaded, or a
// ring that reuses another purpose's key refuses boot.
func eliteaNativeAgentIdentityAtBoot(
	ctx context.Context,
	admission profile.Admission,
	revision profile.PolicyRevision,
	lookup func(string) (string, bool),
	pool *pgxpool.Pool,
	publicProjectID int,
	logger *slog.Logger,
) (eliteaNativeAgentIdentity, error) {
	if !admission.Governed() {
		return eliteaNativeAgentIdentity{}, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	raw, _ := lookup(eliteanative.EnvConfig)
	if strings.TrimSpace(raw) == "" {
		return eliteaNativeAgentIdentity{}, nil
	}
	if !revision.Document.AllowsIdentityMode(agentidentity.IdentityModeEliteaNative) {
		// Gate D2: the mode is used only when the revision lists it. No key
		// is loaded and no route is mounted.
		logger.Warn("Elitea-native agent assertions are configured but the active policy revision does not allow elitea_native; not enabled",
			"policy_revision", revision.ID)
		return eliteaNativeAgentIdentity{}, nil
	}
	deployment, _ := lookup(publicorigin.Env)
	defaultOrigin, _, err := publicorigin.Normalize(deployment)
	if err != nil {
		defaultOrigin = ""
	}
	cfg, err := eliteanative.ParseConfig([]byte(raw), defaultOrigin)
	if err != nil {
		return eliteaNativeAgentIdentity{}, fmt.Errorf("%s: %w", eliteanative.EnvConfig, err)
	}
	foreign, foreignFiles, err := foreignSigningKeys(lookup)
	if err != nil {
		return eliteaNativeAgentIdentity{}, err
	}
	sources := eliteanative.KeySources{}
	for _, key := range cfg.Keys {
		if purpose, ok := foreignFiles[key.File]; ok && key.File != "" {
			return eliteaNativeAgentIdentity{}, fmt.Errorf("%w: key %q is the %s key file", eliteanative.ErrKeyReuse, key.KeyID, purpose)
		}
		if key.Vault != nil && sources.Vault == nil {
			store, _, err := backends.VaultFromEnv(lookup)
			if err != nil {
				return eliteaNativeAgentIdentity{}, fmt.Errorf("agent assertion key ring: %w", err)
			}
			if store == nil {
				return eliteaNativeAgentIdentity{}, errors.New("agent assertion key ring: a key names Vault, but ELITEA_SECRETSTORE_VAULT_* is not configured")
			}
			if publicProjectID <= 0 {
				return eliteaNativeAgentIdentity{}, errors.New("agent assertion key ring: the Vault read needs the platform's public project id")
			}
			sources.Vault = store
			sources.VaultContext = secretstore.StoreContext{
				TenantProjectID: int64(publicProjectID), CredentialRefID: agentAssertionCredentialRef,
				DecisionID: agentAssertionDecision, WorkloadScope: agentAssertionWorkloadScope,
				PolicyRevisionID: revision.ID,
			}
		}
	}
	ring, err := eliteanative.LoadKeyRing(ctx, cfg, sources, foreign)
	if err != nil {
		return eliteaNativeAgentIdentity{}, fmt.Errorf("agent assertion key ring: %w", err)
	}
	audit, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return eliteaNativeAgentIdentity{}, fmt.Errorf("construct agent assertion audit appender: %w", err)
	}
	provider, err := eliteanative.New(cfg, ring, repos.NewExternalAgentIdentities(pool, audit))
	if err != nil {
		return eliteaNativeAgentIdentity{}, err
	}
	kids := make([]string, 0, len(cfg.Keys))
	for _, k := range cfg.Keys {
		kids = append(kids, k.KeyID)
	}
	parties := make([]string, 0, len(cfg.RelyingParties))
	for _, rp := range cfg.RelyingParties {
		parties = append(parties, rp.ID)
	}
	logger.Info("Elitea-native agent assertions enabled", "issuer", cfg.Issuer(), "jwks_uri", cfg.JWKSURI(),
		"relying_parties", parties, "key_ids", kids, "rotation_overlap", cfg.RotationOverlap.String())
	return eliteaNativeAgentIdentity{Provider: provider, WellKnown: provider.WellKnown()}, nil
}

// foreignSigningKeys lists the keys of every other signing purpose this
// process holds, so the ring can refuse to share one: the runtime signing
// key (worker commands and code-sandbox owner grants), the code owner TLS
// identity and the egress token key. It answers their key ids and material
// fingerprints, and their files by path. Each key is read exactly as its
// owning component reads it, for the fingerprint only: a reader stricter
// than the owner's would refuse a mount the owner accepts. A configured key
// that cannot be read refuses boot: the ring's independence could not be
// checked.
func foreignSigningKeys(lookup func(string) (string, bool)) ([]eliteanative.ForeignKey, map[string]string, error) {
	var keys []eliteanative.ForeignKey
	files := map[string]string{}
	add := func(purpose, kid, path string, fingerprint func(string) ([32]byte, error)) error {
		path = strings.TrimSpace(path)
		key := eliteanative.ForeignKey{Purpose: purpose, KeyID: strings.TrimSpace(kid)}
		if path != "" {
			files[path] = purpose
			fp, err := fingerprint(path)
			if err != nil {
				return fmt.Errorf("agent assertion key ring: the %s key cannot be read to check the ring is dedicated: %w", purpose, err)
			}
			key.Fingerprint = fp
		}
		if key.KeyID != "" || path != "" {
			keys = append(keys, key)
		}
		return nil
	}
	kid, _ := lookup("ELITEA_RUNTIME_SIGNING_KEY_ID")
	file, _ := lookup("ELITEA_RUNTIME_SIGNING_KEY_FILE")
	// internal/runtimecomposition reads it with securefile, from the copy
	// the runtime-material init container installs.
	if err := add("command-signing and code-sandbox grant", kid, file, secureFileFingerprint); err != nil {
		return nil, nil, err
	}
	if raw, _ := lookup("ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG"); strings.TrimSpace(raw) != "" {
		var owner struct {
			PrivateKeyPath string `json:"private_key_path"`
		}
		if err := json.Unmarshal([]byte(raw), &owner); err == nil {
			// Read with securefile from the same installed runtime material.
			if err := add("code owner TLS", "", owner.PrivateKeyPath, secureFileFingerprint); err != nil {
				return nil, nil, err
			}
		}
	}
	egress, _ := lookup(egresstoken.EnvSigningKeyFile)
	// The egress minter reads it with runtoken.LoadPrivateKeyFile from a
	// whole-directory Secret volume (defaultMode 0444, kubelet symlinks).
	if err := add("egress token", "", egress, egressFingerprint); err != nil {
		return nil, nil, err
	}
	return keys, files, nil
}

// egressFingerprint reads the egress token key as the egress minter does.
func egressFingerprint(path string) ([32]byte, error) {
	key, err := runtoken.LoadPrivateKeyFile(path)
	if err != nil {
		return [32]byte{}, errors.New("the key file is not readable as an egress token key")
	}
	defer clear(key)
	return eliteanative.Fingerprint(key.Public())
}

// secureFileFingerprint reads a private key file (PKCS #8, SEC 1 or PKCS #1)
// with securefile, as the runtime composition does, and answers its public
// key's fingerprint. The private material is cleared.
func secureFileFingerprint(path string) ([32]byte, error) {
	raw, err := securefile.Read(path, maxForeignKeyFileBytes, securefile.PrivateMaterial)
	if err != nil {
		return [32]byte{}, errors.New("the key file is not readable")
	}
	defer clear(raw)
	for rest := raw; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return [32]byte{}, errors.New("the key file holds no private key")
		}
		var parsed any
		switch block.Type {
		case "PRIVATE KEY":
			parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		case "EC PRIVATE KEY":
			parsed, err = x509.ParseECPrivateKey(block.Bytes)
		case "RSA PRIVATE KEY":
			parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		default:
			continue
		}
		clear(block.Bytes)
		if err != nil {
			return [32]byte{}, errors.New("the key file holds no parseable private key")
		}
		signer, ok := parsed.(crypto.Signer)
		if !ok {
			return [32]byte{}, errors.New("the key file holds no signing key")
		}
		return eliteanative.Fingerprint(signer.Public())
	}
}
