package toolkitdiscovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	call "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

const PayloadTypeToolkitAvailableToolsResult = "TOOLKIT_AVAILABLE_TOOLS_RESULT"

var ErrInvalidDiscovery = errors.New("invalid toolkit discovery request")
var ErrRuntimeFailure = errors.New("toolkit discovery failed")

type Request struct{ ProjectID, ActorUserID, ToolkitID int64 }

func (r Request) Validate() error {
	if r.ProjectID <= 0 || r.ProjectID > math.MaxInt32 || r.ActorUserID <= 0 || r.ActorUserID > math.MaxInt32 || r.ToolkitID <= 0 || r.ToolkitID > math.MaxInt32 {
		return ErrInvalidDiscovery
	}
	return nil
}

type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}
type Result struct {
	Tools       []Tool                     `json:"tools"`
	ArgsSchemas map[string]json.RawMessage `json:"args_schemas"`
}
type UseCase interface {
	AvailableTools(context.Context, Request) (Result, error)
}
type Settlement = call.Settlement
type SettlementReader interface {
	ReadToolkitAvailableToolsSettlement(context.Context, string, uint64) (Settlement, bool, error)
}
type ArtifactReader interface {
	ReadToolkitDiscoveryResult(context.Context, int64, string, uint64, *runtimev1.ToolkitAvailableToolsArtifactReferenceV1) ([]byte, error)
}
type Resolver interface {
	Resolve(context.Context, Request) (AuthoritativeInputs, error)
}
type admissionSubmitter interface {
	Submit(context.Context, SubmitRequest) (AdmittedRun, error)
}
type runDispatcher interface {
	Dispatch(context.Context, Dispatch) error
}
type DispatchPolicy = call.DispatchPolicy
type Service struct {
	resolver    Resolver
	verdict     call.ToolkitTypeVerdict
	admissions  admissionSubmitter
	dispatcher  runDispatcher
	settlements SettlementReader
	artifacts   ArtifactReader
	policy      DispatchPolicy
	newID       executionapp.IDGenerator
	deadline    time.Duration
}

func NewService(resolver Resolver, verdict call.ToolkitTypeVerdict, admissions admissionSubmitter, dispatcher runDispatcher, settlements SettlementReader, artifacts ArtifactReader, policy DispatchPolicy, newID executionapp.IDGenerator, deadline time.Duration) (*Service, error) {
	if resolver == nil || verdict == nil || admissions == nil || dispatcher == nil || settlements == nil || artifacts == nil || newID == nil || policy.CapabilityVersion == "" || policy.ResourceClass == "" || policy.IsolationClass == "" || policy.Priority == 0 || policy.LimitsRevision == "" {
		return nil, errors.New("toolkit discovery dependencies are required")
	}
	if deadline <= 0 {
		deadline = 60 * time.Second
	}
	return &Service{resolver, verdict, admissions, dispatcher, settlements, artifacts, policy, newID, deadline}, nil
}
func (s *Service) AvailableTools(ctx context.Context, request Request) (Result, error) {
	if err := request.Validate(); err != nil {
		return Result{}, err
	}
	inputs, err := s.resolver.Resolve(ctx, request)
	if err != nil {
		return Result{}, err
	}
	if supported, _ := s.verdict.SupportsToolkitType(inputs.ToolkitType); !supported {
		return Result{}, call.ErrUnsupportedToolkitType
	}
	id, err := s.newID()
	if err != nil || id == "" {
		return Result{}, errors.New("allocate toolkit discovery identity")
	}
	project := strconv.FormatInt(request.ProjectID, 10)
	actor := strconv.FormatInt(request.ActorUserID, 10)
	admitted, err := s.admissions.Submit(ctx, SubmitRequest{Identity: executionapp.AdmissionIdentity{TenantID: project, ResourceProjectID: project, ProjectionProjectID: project, ActorID: actor}, IdempotencyKey: "toolkit-discovery-v1:" + id, Inputs: inputs})
	if err != nil {
		return Result{}, err
	}
	if admitted.Outcome.Created {
		err = s.dispatcher.Dispatch(ctx, Dispatch{
			OutboxID: admitted.OutboxID, CommandID: admitted.Outcome.CommandID, ExecutionID: admitted.Outcome.ExecutionID, Generation: 1, DispatchOrdinal: 1, TenantID: project, ResourceProjectID: project, ProjectionProjectID: project, PrincipalRef: actor,
			InputBundleID: admitted.InputBundle.ID, InputBundleVersion: admitted.InputBundle.Version, InputBundleMediaType: admitted.InputBundle.MediaType, InputBundleByteLength: uint64(len(admitted.InputBundle.Manifest)), InputBundleDigest: admitted.InputBundle.Digest,
			CapabilityID: executiondomain.ToolkitAvailableToolsCapability, CapabilityVersion: s.policy.CapabilityVersion, ResourceClass: s.policy.ResourceClass, IsolationClass: s.policy.IsolationClass, Priority: s.policy.Priority, Deadline: admitted.Outcome.Deadline, LimitsRevision: s.policy.LimitsRevision,
			ToolkitType: admitted.Binding.ToolkitType, SettingsEntryID: admitted.Binding.SettingsEntryID,
		})
		if err != nil {
			return Result{}, err
		}
	}
	wait, cancel := context.WithTimeout(ctx, s.deadline)
	defer cancel()
	timer := time.NewTimer(0)
	defer timer.Stop()
	interval := 200 * time.Millisecond
	for {
		select {
		case <-wait.Done():
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			return Result{}, &call.PendingRun{ExecutionID: admitted.Outcome.ExecutionID, Waited: s.deadline}
		case <-timer.C:
		}
		settlement, found, err := s.settlements.ReadToolkitAvailableToolsSettlement(wait, admitted.Outcome.ExecutionID, 1)
		if err != nil {
			return Result{}, err
		}
		if found {
			if settlement.PayloadType != PayloadTypeToolkitAvailableToolsResult || settlement.Outcome != executionapp.SettlementSucceeded {
				return Result{}, ErrRuntimeFailure
			}
			var reference runtimev1.ToolkitAvailableToolsResultV1
			if proto.Unmarshal(settlement.Payload, &reference) != nil || reference.ResultArtifact == nil {
				return Result{}, ErrRuntimeFailure
			}
			content, err := s.artifacts.ReadToolkitDiscoveryResult(wait, request.ProjectID, admitted.Outcome.ExecutionID, 1, reference.ResultArtifact)
			if err != nil {
				return Result{}, err
			}
			var result Result
			if json.Unmarshal(content, &result) != nil || result.Tools == nil || result.ArgsSchemas == nil {
				return Result{}, fmt.Errorf("invalid toolkit discovery result")
			}
			return result, nil
		}
		interval *= 2
		if interval > 2*time.Second {
			interval = 2 * time.Second
		}
		timer.Reset(interval)
	}
}
