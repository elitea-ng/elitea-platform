package adminaccess

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
)

// Refusal is one refused write. It is an error, so a write that runs inside a
// transaction can return it and roll back.
type Refusal struct {
	Code    string
	Message string
	// Role names the role the refusal is about, when there is one.
	Role string
	// Roles names every role a refusal is about when there are several (a
	// matrix save that touches more than one built-in set). Role is the
	// first of them.
	Roles []string
	// Permissions are the permissions the caller lacks, or would gain.
	Permissions []string
	// ProjectID is the project of a CodeSelfEscalation refusal.
	ProjectID int64
	// WaysIn are the ways a caller refused with CodeSelfEscalation can still
	// be given the permissions (WayInProjectMember, WayInOperatorCommand).
	WaysIn []string
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// Body is the JSON body of the 403. `error` keeps the shape every admin client
// already reads; `code` is what a client keys a translated message on.
func (r *Refusal) Body() map[string]any {
	body := map[string]any{"error": r.Message, "code": r.Code}
	if r.Role != "" {
		body["role"] = r.Role
	}
	if len(r.Roles) > 1 {
		body["roles"] = r.Roles
	}
	if len(r.Permissions) > 0 {
		body["permissions"] = r.Permissions
	}
	if r.ProjectID > 0 {
		body["project_id"] = r.ProjectID
	}
	if len(r.WaysIn) > 0 {
		body["ways_in"] = r.WaysIn
		for _, way := range r.WaysIn {
			if way == WayInOperatorCommand {
				body["operator_command"] = OperatorCommand
			}
		}
	}
	return body
}

// Write answers 403 with the refusal, and gives the audit row of the request a
// domain action. The audit middleware records every refused admin write
// (internal/api/middleware/audit.go, event type `api`); without the
// annotation the row says only "PUT /api/v2/admin/permissions/{scope}/{mode}"
// with status 403, which does not say that an escalation was refused.
func Write(w http.ResponseWriter, r *http.Request, refusal *Refusal) {
	annotation := audit.Annotation{
		Action:     RefusalAuditAction,
		EntityType: "role",
		EntityName: refusal.Role,
	}
	if refusal.ProjectID > 0 {
		annotation.ProjectID = audit.ID(refusal.ProjectID)
	}
	audit.Annotate(r.Context(), annotation)
	slog.WarnContext(r.Context(), "role change refused by the no-escalation rule",
		"code", refusal.Code, "role", refusal.Role, "permissions", refusal.Permissions,
		"project_id", refusal.ProjectID, "method", r.Method, "path", r.URL.Path)

	// The headers apierr.WriteStatus sets. Its body carries only `error`, so the
	// refusal keeps its own encoder for `code`, `role` and `permissions`.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(refusal.Body())
}
