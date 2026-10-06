package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

// CodeBrokerPolicy is deployment-owned. Authored Code selects only a boolean.
// Operation permissions remain the existing native actor/project permissions.
type CodeBrokerPolicy struct {
	Revision      uint8  `json:"revision"`
	MaxCalls      uint16 `json:"max_calls"`
	MaxTotalBytes uint32 `json:"max_total_bytes"`
}

func (p CodeBrokerPolicy) SHA256() (string, error) {
	if p.Revision != 1 || p.MaxCalls < 1 || p.MaxCalls > 4096 || p.MaxTotalBytes < 1 || p.MaxTotalBytes > 67108864 {
		return "", code.ErrRejected
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", code.ErrRejected
	}
	hash := sha256.New()
	hash.Write([]byte("elitea.code.platform-policy.v1\x00"))
	hash.Write(raw)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type CodeBrokerPolicies struct {
	policies map[string]CodeBrokerPolicy
	effects  CodeBrokerEffectReader
}

// NewCodeBrokerPolicies is called only by trusted Main deployment composition.
// A missing verifier or registry keeps authored platform_client denied.
func NewCodeBrokerPolicies(entries []CodeBrokerPolicy) (*CodeBrokerPolicies, error) {
	if len(entries) < 1 || len(entries) > 16 {
		return nil, code.ErrRejected
	}
	result := &CodeBrokerPolicies{policies: make(map[string]CodeBrokerPolicy, len(entries))}
	for _, entry := range entries {
		digest, err := entry.SHA256()
		if err != nil {
			return nil, err
		}
		if _, exists := result.policies[digest]; exists {
			return nil, code.ErrRejected
		}
		result.policies[digest] = entry
	}
	return result, nil
}
func (p *CodeBrokerPolicies) VerifyOriginalCodeBroker(ctx context.Context, tx CodeTransaction, claim ContentClaim, original OriginalCodeVisit, prepared CodePreparedRequest) error {
	if p == nil || ctx == nil || tx == nil || claim.PeerCertificate == nil || claim.ExecutionID != original.ExecutionID || !original.Declaration.PlatformClient || original.Reference.Validate() != nil || prepared.Revision != 5 || prepared.Broker == nil {
		return code.ErrRejected
	}
	broker := prepared.Broker
	policy, found := p.policies[broker.PolicySHA256]
	if !found || broker.Revision != 1 || broker.MaxCalls != policy.MaxCalls || broker.MaxTotalBytes != policy.MaxTotalBytes {
		return code.ErrRejected
	}
	raw, err := json.Marshal(broker)
	if err != nil {
		return code.ErrRejected
	}
	// The caller already holds current claim/original definition locks. No IO or
	// platform dispatch occurs here; this is the final immutable capability pin.
	_, err = tx.Exec(ctx, `INSERT INTO elitea_runtime.original_code_broker_bindings(execution_id,original_generation,visit_id,prepared_sha256,prepared_fingerprint,policy_sha256,broker_json,dependency_bundle_sha256)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, original.ExecutionID, int64(original.OriginalGeneration), original.Reference.VisitID, prepared.PreparedSHA256, prepared.Fingerprint, broker.PolicySHA256, raw, prepared.DependencyBundleSHA256)
	if err != nil {
		return err
	}
	var actualRaw []byte
	var actualSHA, actualFingerprint, actualPolicy, actualBundle string
	err = tx.QueryRow(ctx, `SELECT prepared_sha256,prepared_fingerprint,policy_sha256,broker_json,dependency_bundle_sha256 FROM elitea_runtime.original_code_broker_bindings
WHERE execution_id=$1 AND original_generation=$2 AND visit_id=$3 FOR UPDATE`, original.ExecutionID, int64(original.OriginalGeneration), original.Reference.VisitID).Scan(&actualSHA, &actualFingerprint, &actualPolicy, &actualRaw, &actualBundle)
	if err != nil || actualSHA != prepared.PreparedSHA256 || actualFingerprint != prepared.Fingerprint || actualPolicy != broker.PolicySHA256 || actualBundle != prepared.DependencyBundleSHA256 || !bytes.Equal(actualRaw, raw) {
		return code.ErrRejected
	}
	return nil
}

type RegisteredCodeBrokerBinding struct {
	PreparedSHA256, PreparedFingerprint, DependencyBundleSHA256 string
	Broker                                                      CodePreparedPlatformClient
}

// ReadRegisteredCodeBroker runs only in the original registered intent callback.
func ReadRegisteredCodeBroker(ctx context.Context, tx CodeTransaction, intent RegisteredCodeIntent) (RegisteredCodeBrokerBinding, error) {
	var result RegisteredCodeBrokerBinding
	var raw []byte
	if ctx == nil || tx == nil || intent.Binding.Validate() != nil || !intent.Original.Declaration.PlatformClient {
		return result, code.ErrRejected
	}
	err := tx.QueryRow(ctx, `SELECT prepared_sha256,prepared_fingerprint,broker_json,dependency_bundle_sha256 FROM elitea_runtime.original_code_broker_bindings
WHERE execution_id=$1 AND original_generation=$2 AND visit_id=$3 FOR SHARE`, intent.Original.ExecutionID, int64(intent.Original.OriginalGeneration), intent.Original.Reference.VisitID).Scan(&result.PreparedSHA256, &result.PreparedFingerprint, &raw, &result.DependencyBundleSHA256)
	if err != nil || len(raw) > 512 || codePreparedDecode(raw, &result.Broker) != nil || result.PreparedSHA256 != intent.Binding.PreparedSHA256 || result.PreparedFingerprint != intent.PreparedFingerprint || result.Broker.Revision != 1 || result.Broker.MaxCalls < 1 || result.Broker.MaxCalls > 4096 || result.Broker.MaxTotalBytes < 1 || result.Broker.MaxTotalBytes > 67108864 || !workspaceHex(result.Broker.PolicySHA256, 64) {
		return RegisteredCodeBrokerBinding{}, code.ErrRejected
	}
	if VerifyCodeBrokerRequestIdentity(intent, result, intent.CompiledExecute) != nil {
		return RegisteredCodeBrokerBinding{}, code.ErrRejected
	}
	return result, nil
}

// The actual journal reader is attached by trusted Main composition. Missing
// registration or an absent reader cannot prove a broker-enabled job had no effect.
func (p *CodeBrokerPolicies) WithEffectJournal(reader CodeBrokerEffectReader) *CodeBrokerPolicies {
	if p != nil {
		p.effects = reader
	}
	return p
}
func (p *CodeBrokerPolicies) ReadOriginalCodeBrokerEffects(ctx context.Context, tx CodeTransaction, execution string, generation uint64, dispatch, rawSHA, fingerprint string) (CodeBrokerEffectFacts, error) {
	if p == nil || p.effects == nil {
		return CodeBrokerEffectFacts{}, code.ErrRejected
	}
	return p.effects.ReadOriginalCodeBrokerEffects(ctx, tx, execution, generation, dispatch, rawSHA, fingerprint)
}

var _ CodeBrokerEffectReader = (*CodeBrokerPolicies)(nil)
