package toolkitcalltool

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	codeplatform "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"strconv"
)

var ErrCodeToolkitRevisionConflict = errors.New("code toolkit revision changed")
var ErrCodeToolApprovalRequired = errors.New("code toolkit sensitive approval required")
var ErrCodeToolPolicyDenied = errors.New("code toolkit policy denied")

// CheckCodeTool evaluates the existing frozen native guardrails before admission.
// No user argument or broker field can represent sensitive approval.
func (s *RunService) CheckCodeTool(ctx context.Context, request RunRequest, revision string) error {
	if s == nil || ctx == nil || request.Validate() != nil {
		return ErrInvalidToolRun
	}
	inputs, err := s.resolver.Resolve(ctx, request.Clone())
	if err != nil {
		return err
	}
	sum := CodeToolkitRevision(inputs)
	if hex.EncodeToString(sum[:]) != revision {
		return ErrCodeToolkitRevisionConflict
	}
	var frozen RuntimeContext
	if !validRuntimeContext(inputs.RuntimeContext) || json.Unmarshal(inputs.RuntimeContext, &frozen) != nil || frozen.ToolkitSecurity == nil {
		return ErrToolkitSettingsResolutionUnavailable
	}
	runtime := frozen.ToolkitSecurity
	policy := guardrails.NewPolicy(guardrails.PolicyInput{BlockedToolkits: runtime.BlockedToolkits, BlockedTools: runtime.BlockedTools, SensitiveTools: runtime.SensitiveTools, CompanyName: runtime.CompanyName, MessageTemplate: runtime.MessageTemplate})
	var config struct {
		Name string `json:"toolkit_name"`
	}
	if json.Unmarshal(inputs.Settings, &config) != nil {
		return ErrInvalidAuthoritativeToolRunInput
	}
	if policy.ToolBlocked(inputs.ToolkitType, request.ToolName) || policy.ToolBlocked(config.Name, request.ToolName) {
		return ErrCodeToolPolicyDenied
	}
	if _, sensitive := policy.SensitiveMatch(request.ToolName, inputs.ToolkitType, config.Name); sensitive {
		return ErrCodeToolApprovalRequired
	}
	return nil
}

// CodeToolkitRevision binds the exact saved ID, type, version, and frozen settings.
// It excludes caller arguments and redeemed credentials.
func CodeToolkitRevision(inputs AuthoritativeInputs) [32]byte {
	hash := sha256.New()
	hash.Write([]byte("elitea.code.toolkit-revision.v1\x00"))
	for _, field := range [][]byte{[]byte(strconv.FormatInt(inputs.ToolkitID, 10)), []byte(inputs.ToolkitType), []byte(inputs.ToolkitVersion), inputs.Settings} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		hash.Write(length[:])
		hash.Write(field)
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

// ResolveCodeToolkitRevision selects one visible toolkit through the native resolver.
// The caller cannot provide settings or select a toolkit by name.
func (s *RunService) ResolveCodeToolkitRevision(ctx context.Context, request RunRequest) ([32]byte, error) {
	if s == nil || ctx == nil || request.Validate() != nil {
		return [32]byte{}, ErrInvalidToolRun
	}
	if err := ctx.Err(); err != nil {
		return [32]byte{}, err
	}
	inputs, err := s.resolver.Resolve(ctx, request.Clone())
	if err != nil {
		return [32]byte{}, err
	}
	return CodeToolkitRevision(inputs), nil
}

// RunExactTool checks the selected revision before durable admission.
// The native service still owns grants, guardrails, dispatch, and settlement.
func (s *RunService) RunExactTool(ctx context.Context, request RunRequest, revision string) (RunOutcome, error) {
	var digest [32]byte
	if len(revision) != 64 {
		return RunOutcome{}, ErrInvalidToolRun
	}
	decoded, err := hex.DecodeString(revision)
	if err != nil || hex.EncodeToString(decoded) != revision {
		return RunOutcome{}, ErrInvalidToolRun
	}
	copy(digest[:], decoded)
	return s.runTool(ctx, request, &digest, nil)
}

// RunCodeTool admits one child under the immutable Code effect.
// The native repository commits the child ownership relation with admission.
func (s *RunService) RunCodeTool(ctx context.Context, request RunRequest, revision string, parent codeplatform.ParentEffect) (RunOutcome, error) {
	if parent.Validate() != nil || request.ProjectID != parent.Admission.Job.ProjectID || request.ActorUserID != parent.Admission.Job.ActorID || request.IdempotencyKey != "code-platform-"+parent.EffectID {
		return RunOutcome{}, ErrInvalidToolRun
	}
	decoded, err := hex.DecodeString(revision)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != revision {
		return RunOutcome{}, ErrInvalidToolRun
	}
	var digest [32]byte
	copy(digest[:], decoded)
	return s.runTool(ctx, request, &digest, &parent)
}
