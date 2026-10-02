package repos

import (
	"context"
	"fmt"
	"strconv"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkit "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
)

// ToolkitCallToolEnvelopePreparer signs a reference command without publishing it.
// Production admission supplies this dependency. Omission supports legacy rows in tests.
type ToolkitCallToolEnvelopePreparer interface {
	PrepareToolkitCallTool(context.Context, toolkit.Dispatch) (executionapp.PreparedCommandEnvelope, error)
}

func (r *ToolkitCallToolJobsRepository) prepareAdmissionEnvelope(ctx context.Context, admission toolkit.Admission, deadline time.Time) (executionapp.PreparedCommandEnvelope, error) {
	record, binding := admission.Record, admission.Binding
	dispatch := toolkit.Dispatch{
		OutboxID: record.Outbox.ID, CommandID: record.Job.CommandID, ExecutionID: record.Job.ID,
		Generation: record.Job.Generation, DispatchOrdinal: 1,
		TenantID: record.Job.TenantID, ResourceProjectID: record.Job.ResourceProjectID,
		ProjectionProjectID: record.Job.ProjectionProjectID, PrincipalRef: record.Job.ActorID,
		InputBundleID: record.InputBundle.ID, InputBundleVersion: record.InputBundle.Version,
		InputBundleMediaType: record.InputBundle.MediaType, InputBundleByteLength: uint64(len(record.InputBundle.Manifest)),
		InputBundleDigest: record.InputBundle.Digest, CapabilityID: record.Job.CapabilityID,
		CapabilityVersion: r.policy.CapabilityVersion, ResourceClass: r.policy.ResourceClass,
		IsolationClass: r.policy.IsolationClass, Priority: uint32(r.policy.Priority), Deadline: deadline,
		LimitsRevision: r.policy.LimitsRevision, ToolkitType: binding.ToolkitType, ToolName: binding.ToolName,
		ToolkitID: strconv.FormatInt(binding.ToolkitID, 10), ToolkitVersion: binding.ToolkitVersion,
		SettingsEntryID: binding.SettingsEntryID, ArgumentsEntryID: binding.ArgumentsEntryID,
	}
	if err := dispatch.Validate(); err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	envelope, err := r.preparer.PrepareToolkitCallTool(ctx, dispatch)
	if err != nil {
		return executionapp.PreparedCommandEnvelope{}, fmt.Errorf("prepare admitted tool-run envelope: %w", err)
	}
	if err := envelope.Validate(); err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	return envelope, nil
}
