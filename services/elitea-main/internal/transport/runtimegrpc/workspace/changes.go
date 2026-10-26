package workspace

import (
	"errors"
	"io"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
)

// maxChangeFrames bounds the record stream: one header, then at most
// MaxChangedFiles files per scope.
const maxChangeFrames = 1 + 2*workspaceruntime.MaxChangedFiles

// WorkspaceRecordChanges keeps the files the running turn's published commit
// changed (ADR-0034 Phase 2): one header frame, then one frame per file. The
// whole record is read and checked before the claim's thread is asked; a
// stream longer than the bounds is malformed.
func (s *Server) WorkspaceRecordChanges(stream runtimev1.WorkspaceStoreService_WorkspaceRecordChangesServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	header := first.GetHeader()
	if header == nil {
		return stream.SendAndClose(&runtimev1.WorkspaceRecordChangesResponseV1{Rejection: malformed()})
	}
	changes := workspaceruntime.TurnChanges{
		Commit: header.GetCommit(), Parent: header.GetParent(), Base: header.GetBase(),
		TurnOmitted: header.GetTurnOmitted(), CumulativeOmitted: header.GetCumulativeOmitted(),
	}
	for frames := 1; ; frames++ {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		file := frame.GetFile()
		if file == nil || frames >= maxChangeFrames {
			return stream.SendAndClose(&runtimev1.WorkspaceRecordChangesResponseV1{Rejection: malformed()})
		}
		changed, ok := changedFile(file)
		if !ok {
			return stream.SendAndClose(&runtimev1.WorkspaceRecordChangesResponseV1{Rejection: malformed()})
		}
		changes.Files = append(changes.Files, changed)
	}
	if err := workspaceruntime.ValidateTurnChanges(changes); err != nil {
		return stream.SendAndClose(&runtimev1.WorkspaceRecordChangesResponseV1{Rejection: malformed()})
	}
	claim, reject := s.authorize(ctx, header.GetClaim())
	if reject != nil {
		return stream.SendAndClose(&runtimev1.WorkspaceRecordChangesResponseV1{Rejection: reject})
	}
	if err := s.store.RecordChanges(ctx, claim, changes); err != nil {
		if refusal, ok := refusalProto(err); ok {
			return stream.SendAndClose(&runtimev1.WorkspaceRecordChangesResponseV1{Refusal: refusal})
		}
		return stream.SendAndClose(&runtimev1.WorkspaceRecordChangesResponseV1{Rejection: s.storeRejection(ctx, "record_changes", err)})
	}
	return stream.SendAndClose(&runtimev1.WorkspaceRecordChangesResponseV1{Recorded: true})
}

func changedFile(file *runtimev1.WorkspaceChangedFileV1) (workspaceruntime.ChangedFile, bool) {
	out := workspaceruntime.ChangedFile{
		Path: file.GetPath(), FromPath: file.GetFromPath(), Binary: file.GetBinary(), TooLarge: file.GetTooLarge(), Diff: file.GetDiff(),
	}
	switch file.GetScope() {
	case runtimev1.WorkspaceChangeScopeV1_WORKSPACE_CHANGE_SCOPE_V1_TURN:
		out.Scope = workspaceruntime.ChangeScopeTurn
	case runtimev1.WorkspaceChangeScopeV1_WORKSPACE_CHANGE_SCOPE_V1_CUMULATIVE:
		out.Scope = workspaceruntime.ChangeScopeCumulative
	default:
		return workspaceruntime.ChangedFile{}, false
	}
	switch file.GetStatus() {
	case runtimev1.WorkspaceChangeStatusV1_WORKSPACE_CHANGE_STATUS_V1_ADDED:
		out.Status = workspaceruntime.ChangeAdded
	case runtimev1.WorkspaceChangeStatusV1_WORKSPACE_CHANGE_STATUS_V1_MODIFIED:
		out.Status = workspaceruntime.ChangeModified
	case runtimev1.WorkspaceChangeStatusV1_WORKSPACE_CHANGE_STATUS_V1_DELETED:
		out.Status = workspaceruntime.ChangeDeleted
	case runtimev1.WorkspaceChangeStatusV1_WORKSPACE_CHANGE_STATUS_V1_RENAMED:
		out.Status = workspaceruntime.ChangeRenamed
	default:
		return workspaceruntime.ChangedFile{}, false
	}
	if file.Additions != nil {
		value := file.GetAdditions()
		out.Additions = &value
	}
	if file.Deletions != nil {
		value := file.GetDeletions()
		out.Deletions = &value
	}
	return out, true
}
