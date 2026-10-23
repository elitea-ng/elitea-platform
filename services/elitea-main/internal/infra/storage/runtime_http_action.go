package storage

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// HTTPActionRule is operator-owned authority for one exact operation.
// YAML cannot provide the policy subject, credential revision, or artifact bucket.
type HTTPActionRule struct {
	ProjectID          int64
	ActorID            int64
	Origin             string
	Path               string
	Method             string
	Toolkit            string
	Tool               string
	ConfigurationID    int32
	CredentialRevision string
	OutputBucket       string
}
type HTTPActionInput struct {
	Payload             []byte
	ProjectID           int64
	ActorID             int64
	TenantID            string
	ProjectionProjectID int64
	ClaimID             string
	LeaseEpoch          uint64
	Deadline            time.Time
}
type HTTPActionSource interface {
	ReadHTTPActionInput(context.Context, ContentClaim) (HTTPActionInput, error)
}
type RuntimeHTTPActionService struct {
	source      HTTPActionSource
	effects     app.Effects
	credentials app.Credentials
	artifacts   app.Artifacts
	transport   app.Transport
	rules       []HTTPActionRule
}

func NewRuntimeHTTPActionService(source HTTPActionSource, effects app.Effects, credentials app.Credentials, artifacts app.Artifacts, transport app.Transport, rules []HTTPActionRule) (*RuntimeHTTPActionService, error) {
	if source == nil || effects == nil || transport == nil || len(rules) == 0 || len(rules) > 128 {
		return nil, app.ErrUnavailable
	}
	for _, rule := range rules {
		u, e := url.Parse(rule.Origin)
		if rule.ProjectID <= 0 || rule.ActorID <= 0 || e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || rule.Path == "" || rule.Toolkit == "" || rule.Tool == "" || rule.ConfigurationID < 0 || rule.ConfigurationID > 0 && !app.ValidDigest(rule.CredentialRevision) {
			return nil, app.ErrInvalid
		}
	}
	return &RuntimeHTTPActionService{source: source, effects: effects, credentials: credentials, artifacts: artifacts, transport: transport, rules: append([]HTTPActionRule(nil), rules...)}, nil
}
func (s *RuntimeHTTPActionService) Execute(ctx context.Context, claim ContentClaim, inv app.Invocation) (app.Receipt, error) {
	admitted, err := s.source.ReadHTTPActionInput(ctx, claim)
	if err != nil {
		return app.Receipt{}, app.ErrUnauthorized
	}
	runner, err := app.New(httpAdmitter{input: admitted, claim: claim, rules: s.rules}, s.effects, s.credentials, s.artifacts, s.transport)
	if err != nil {
		return app.Receipt{}, err
	}
	return runner.Execute(ctx, inv)
}

type httpAdmitter struct {
	input HTTPActionInput
	claim ContentClaim
	rules []HTTPActionRule
}

func (a httpAdmitter) Admit(_ context.Context, inv app.Invocation, request app.Request) (app.Admission, error) {
	var input runtimev1.AgentExecutionInputV1
	if len(a.input.Payload) > 8*1024*1024 || proto.Unmarshal(a.input.Payload, &input) != nil || len(input.ToolkitGuardrails) == 0 {
		return app.Admission{}, app.ErrUnauthorized
	}
	var application struct {
		ID             int64 `json:"id"`
		VersionID      int64 `json:"version_id"`
		VersionDetails struct {
			Instructions string             `json:"instructions"`
			Snapshot     app.FrozenSnapshot `json:"http_action_snapshot"`
		} `json:"version_details"`
	}
	if json.Unmarshal(input.Application, &application) != nil || application.VersionDetails.Snapshot.Verify(application.ID, application.VersionID, application.VersionDetails.Instructions, inv) != nil {
		return app.Admission{}, app.ErrUnauthorized
	}
	// The frozen contract admits only the root graph. Child scope contracts remain deferred.
	if inv.ThreadID != rootGraphThread(a.input.TenantID, a.input.ProjectID, a.input.ProjectionProjectID, input.GetThreadId()) {
		return app.Admission{}, app.ErrUnauthorized
	}
	var frozen guardrails.RuntimePolicy
	if json.Unmarshal(input.ToolkitGuardrails, &frozen) != nil {
		return app.Admission{}, app.ErrUnauthorized
	}
	policy := guardrails.NewPolicy(guardrails.PolicyInput{BlockedToolkits: frozen.BlockedToolkits, BlockedTools: frozen.BlockedTools, SensitiveTools: frozen.SensitiveTools})
	u, err := url.Parse(request.URL)
	if err != nil {
		return app.Admission{}, app.ErrUnauthorized
	}
	configurationID := int32(0)
	if request.Credential != nil {
		configurationID = request.Credential.ConfigurationID
	}
	var selected *HTTPActionRule
	for i := range a.rules {
		rule := &a.rules[i]
		if rule.ProjectID == a.input.ProjectID && rule.ActorID == a.input.ActorID && rule.Origin == u.Scheme+"://"+u.Host && rule.Path == u.EscapedPath() && rule.Method == request.Method && rule.ConfigurationID == configurationID {
			if selected != nil {
				return app.Admission{}, app.ErrUnauthorized
			}
			selected = rule
		}
	}
	if selected == nil || policy.ToolkitBlocked(selected.Toolkit) || policy.ToolBlocked(selected.Toolkit, selected.Tool) {
		return app.Admission{}, app.ErrUnauthorized
	}
	// No alternate sensitive or delegated decision is invented by the HTTP path.
	// Until call-bound HTTP approval is integrated, sensitive actions fail closed.
	if _, sensitive := policy.SensitiveMatch(selected.Tool, selected.Toolkit); sensitive {
		return app.Admission{}, app.ErrUnauthorized
	}
	return app.Admission{ExecutionID: a.claim.ExecutionID, Generation: a.claim.Generation, ProjectID: a.input.ProjectID, ActorID: a.input.ActorID, ClaimID: a.input.ClaimID, LeaseEpoch: a.input.LeaseEpoch, Deadline: a.input.Deadline, CredentialRevision: selected.CredentialRevision, OutputBucket: selected.OutputBucket, PolicyDigest: httpRuleDigest(*selected)}, nil
}
func rootGraphThread(tenant string, project, projection int64, thread string) string {
	return app.RootGraphThread(tenant, project, projection, thread)
}

// Main policy ownership is pinned in the durable effect alongside frozen bytes.
func httpRuleDigest(rule HTTPActionRule) string {
	raw, _ := json.Marshal(rule)
	return app.Digest(raw)
}

func (s *ContentServer) WithRuntimeHTTPActions(actions *RuntimeHTTPActionService) *ContentServer {
	if s != nil {
		s.runtimeHTTPActions = actions
	}
	return s
}

// ExecuteHTTPAction runs one claim-scoped HTTP action invocation.
func (s *ContentServer) ExecuteHTTPAction(ctx context.Context, request *runtimev1.ExecuteHTTPActionRequestV1) (*runtimev1.ExecuteHTTPActionResponseV1, error) {
	if s.runtimeHTTPActions == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	release, err := acquireContentSlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, contentStatus(codes.Unauthenticated)
	}
	data := request.GetInvocationJson()
	if len(data) > app.MaxInvocation {
		return nil, contentStatus(codes.InvalidArgument)
	}
	var invocation app.Invocation
	if app.Decode(data, &invocation) != nil {
		return nil, contentStatus(codes.InvalidArgument)
	}
	receipt, err := s.runtimeHTTPActions.Execute(ctx, claim, invocation)
	if err != nil {
		switch {
		case errors.Is(err, app.ErrUnauthorized):
			return nil, contentStatus(codes.PermissionDenied)
		case errors.Is(err, app.ErrInvalid):
			return nil, contentStatus(codes.FailedPrecondition)
		}
		return nil, contentStatus(codes.Unavailable)
	}
	body, err := json.Marshal(receipt)
	if err != nil || len(body) > 3*1024*1024 {
		return nil, contentStatus(codes.Unavailable)
	}
	return &runtimev1.ExecuteHTTPActionResponseV1{ReceiptJson: body}, nil
}
