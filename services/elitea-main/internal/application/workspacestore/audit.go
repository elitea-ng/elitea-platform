package workspacestore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
)

// AuditEntityType is the entity type of every handoff and store audit event
// (decision 12).
const AuditEntityType = "workspace_handoff"

// RecorderAudit writes store events to the audit trail. Events record counts,
// sizes, shas, writers and reasons, never file contents or paths.
type RecorderAudit struct {
	Recorder audit.Recorder
	Now      func() time.Time
}

func (a RecorderAudit) StoreEvent(ctx context.Context, event AuditEvent) {
	if a.Recorder == nil {
		return
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	parts := []string{event.Name}
	if event.Writer != "" {
		parts = append(parts, "writer "+string(event.Writer))
	}
	if event.Epoch > 0 {
		parts = append(parts, fmt.Sprintf("epoch %d", event.Epoch))
	}
	if event.Seq > 0 {
		parts = append(parts, fmt.Sprintf("seq %d", event.Seq))
	}
	if event.SHA256 != "" {
		parts = append(parts, "segment "+event.SHA256)
	}
	if event.Bytes > 0 {
		parts = append(parts, fmt.Sprintf("%d bytes", event.Bytes))
	}
	if event.Count > 0 {
		parts = append(parts, fmt.Sprintf("count %d", event.Count))
	}
	if event.Reason != "" {
		parts = append(parts, "reason "+event.Reason)
	}
	action := strings.Join(parts, ", ")
	if runes := []rune(action); len(runes) > 512 {
		action = string(runes[:511]) + "…"
	}
	status := int32(200)
	if !event.Succeeded && event.Name != "store.gc" {
		status = 409
	}
	duration := float64(now().Sub(event.Started).Microseconds()) / 1000
	recorded := audit.Event{
		Timestamp: now().UTC(), EventType: "agent", Action: action,
		StatusCode: &status, DurationMS: &duration, IsError: status >= 400,
		EntityType: AuditEntityType, EntityName: event.Thread.ConversationUUID,
	}
	if event.Name == "store.gc" {
		recorded.EventType = "schedule"
	}
	if event.UserID > 0 {
		recorded.UserID = audit.ID(event.UserID)
	}
	if event.Thread.ProjectID > 0 {
		recorded.ProjectID = audit.ID(event.Thread.ProjectID)
	}
	a.Recorder.Record(ctx, recorded)
}
