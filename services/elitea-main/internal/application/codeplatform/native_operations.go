package codeplatform

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	toolkit "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
)

type SecretReader interface {
	AuthorizeCodeSecretRead(context.Context, domain.Admission, string, string) error
	ReadCodeSecret(context.Context, domain.Admission, string, string) (Outcome, error)
}
type CatalogReader interface {
	AuthorizeCodeReply(context.Context, domain.Admission, domain.Request, domain.Reply) error
	ReadCodeCatalog(context.Context, domain.Admission, domain.Request) (Outcome, error)
	AuthorizeCodeToolkitCall(context.Context, domain.Admission) error
}
type ToolkitChildren interface {
	ReadCodeToolkitChild(context.Context, domain.Admission, domain.Record) (domain.ToolkitChild, bool, error)
}

// NativeOperations reuses the saved toolkit executor and secret/catalog owners.
// General artifact transfer, append, and delete remain a separate Gate7 contract.
type NativeOperations struct {
	tools    *toolkit.RunService
	children ToolkitChildren
	secrets  SecretReader
	catalog  CatalogReader
}

func NewNativeOperations(tools *toolkit.RunService, children ToolkitChildren, secrets SecretReader, catalog CatalogReader) (*NativeOperations, error) {
	if tools == nil || children == nil || secrets == nil || catalog == nil {
		return nil, domain.ErrUnavailable
	}
	return &NativeOperations{tools: tools, children: children, secrets: secrets, catalog: catalog}, nil
}
func known(status string) Outcome {
	return Outcome{Known: true, Status: status, Result: json.RawMessage("null")}
}
func (n *NativeOperations) Execute(ctx context.Context, a domain.Admission, record domain.Record, request domain.Request) (Outcome, error) {
	if n == nil || ctx == nil || a.Validate() != nil {
		return Outcome{}, domain.ErrUnauthorized
	}
	switch request.Operation {
	case "secret_read":
		return n.secrets.ReadCodeSecret(ctx, a, request.Resource["scope"], request.Resource["name"])
	case "application_list", "application_get", "application_version_get", "user_get", "toolkit_list", "bucket_exists", "bucket_create", "artifact_list", "artifact_head":
		return n.catalog.ReadCodeCatalog(ctx, a, request)
	case "toolkit_call":
		if err := n.catalog.AuthorizeCodeToolkitCall(ctx, a); err != nil {
			if errors.Is(err, domain.ErrUnauthorized) {
				return known("authorization_denied"), nil
			}
			return known("dependency_unavailable"), nil
		}
		call, err := nativeToolRequest(a, record, request)
		if err != nil {
			return known("invalid_resource"), nil
		}
		if err = n.tools.CheckCodeTool(ctx, call, request.Resource["revision"]); err != nil {
			switch {
			case errors.Is(err, toolkit.ErrCodeToolApprovalRequired):
				return known("approval_required"), nil
			case errors.Is(err, toolkit.ErrCodeToolPolicyDenied), errors.Is(err, toolkit.ErrToolkitNotVisible):
				return known("authorization_denied"), nil
			case errors.Is(err, toolkit.ErrCodeToolkitRevisionConflict):
				return known("revision_conflict"), nil
			default:
				return known("dependency_unavailable"), nil
			}
		}
		result, err := n.tools.RunCodeTool(ctx, call, request.Resource["revision"], domain.ParentEffect{Admission: a, Intent: record.Intent, EffectID: record.EffectID})
		if err != nil {
			return Outcome{}, domain.ErrUnknown
		}
		if result.ExecutionID == "" {
			if result.Status == toolkit.RunStatusUnsupportedToolkit {
				return known("unsupported_operation"), nil
			}
			return Outcome{}, domain.ErrUnknown
		}
		child, found, err := n.children.ReadCodeToolkitChild(ctx, a, record)
		if err != nil || !found || !child.Settled || child.ExecutionID != result.ExecutionID || child.ToolkitID != call.ToolkitID || child.Revision != request.Resource["revision"] {
			return Outcome{}, domain.ErrUnknown
		}
		return toolOutcome(result, child.OwnerReceipt)
	default:
		// Never approximate append/delete with a nonconditional backend write.
		return known("unsupported_operation"), nil
	}
}
func nativeToolRequest(a domain.Admission, record domain.Record, request domain.Request) (toolkit.RunRequest, error) {
	id, err := strconv.ParseInt(request.Resource["id"], 10, 32)
	if err != nil || id < 1 || record.Intent.Operation != "toolkit_call" || record.EffectID != domain.EffectID(a.Job, record.Intent) {
		return toolkit.RunRequest{}, domain.ErrConflict
	}
	call := toolkit.RunRequest{ProjectID: a.Job.ProjectID, ActorUserID: a.Job.ActorID, ToolkitID: id, ToolName: request.Resource["tool"], Arguments: append(json.RawMessage(nil), request.Arguments...), IdempotencyKey: "code-platform-" + record.EffectID}
	return call, call.Validate()
}
func (n *NativeOperations) Reconcile(ctx context.Context, a domain.Admission, record domain.Record, request domain.Request) (Outcome, bool, error) {
	if n == nil || request.Operation != "toolkit_call" {
		return Outcome{}, false, domain.ErrUnknown
	}
	child, found, err := n.children.ReadCodeToolkitChild(ctx, a, record)
	if err != nil {
		return Outcome{}, false, err
	}
	if !found || !child.Settled {
		return Outcome{}, false, nil
	}
	id, err := strconv.ParseInt(request.Resource["id"], 10, 32)
	if err != nil || id != child.ToolkitID || child.Revision != request.Resource["revision"] || child.ArgumentsSHA256 != record.Intent.Arguments {
		return Outcome{}, false, domain.ErrConflict
	}
	// ReadToolRun accepts only the stored exact child ID and never calls submission.
	result, pending, err := n.tools.ReadToolRun(ctx, toolkit.ResultRequest{ProjectID: a.Job.ProjectID, ActorUserID: a.Job.ActorID, ToolkitID: child.ToolkitID, ExecutionID: child.ExecutionID})
	if err != nil {
		return Outcome{}, false, err
	}
	if pending {
		return Outcome{}, false, nil
	}
	outcome, err := toolOutcome(result, child.OwnerReceipt)
	return outcome, err == nil, err
}
func toolOutcome(result toolkit.RunOutcome, owner string) (Outcome, error) {
	outcome := known("dependency_unavailable")
	outcome.OwnerReceipt = owner
	switch result.Status {
	case toolkit.RunStatusOK:
		if result.Truncated || len(result.ResultJSON) > domain.MaxReplyHeader || !json.Valid([]byte(result.ResultJSON)) {
			return Outcome{}, domain.ErrUnknown
		}
		outcome.Status = "ok"
		outcome.Result = json.RawMessage(result.ResultJSON)
	case toolkit.RunStatusAuthorizationRequired:
		outcome.Status = "authentication_denied"
	case toolkit.RunStatusUnsupportedToolkit, toolkit.RunStatusUnknownTool:
		outcome.Status = "unsupported_operation"
	case toolkit.RunStatusToolError, toolkit.RunStatusRuntimeFailure:
	default:
		return Outcome{}, domain.ErrUnknown
	}
	return outcome, nil
}

// AuthorizeReply checks current resource visibility before releasing old bytes.
// Negative replies contain no data; the journal signer still checks the claim.
func (n *NativeOperations) AuthorizeReply(ctx context.Context, a domain.Admission, record domain.Record, request domain.Request, reply domain.Reply) error {
	if n == nil || ctx == nil || a.Validate() != nil {
		return domain.ErrUnauthorized
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if reply.Status != "ok" && reply.Status != "not_found" {
		return nil
	}
	if request.Operation == "secret_read" {
		return n.secrets.AuthorizeCodeSecretRead(ctx, a, request.Resource["scope"], request.Resource["name"])
	}
	if request.Operation == "toolkit_call" {
		if err := n.catalog.AuthorizeCodeToolkitCall(ctx, a); err != nil {
			return err
		}
		call, err := nativeToolRequest(a, record, request)
		if err != nil {
			return err
		}
		return n.tools.CheckCodeTool(ctx, call, request.Resource["revision"])
	}
	return n.catalog.AuthorizeCodeReply(ctx, a, request, reply)
}
