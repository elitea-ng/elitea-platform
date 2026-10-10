package projects

// Project creation — the route that made a pylon-free deployment able to onboard
// a tenant (#333). The reference is
// legacy/plugins/projects/api/v2/project.py's AdminAPI.post.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// CreateProjectPermission gates the create route.
//
// The name is transcribed from the reference's `check_api` declaration and is
// present in testdata/legacy/legacy-rbac-static-catalog.json. Nothing in this
// corpus granted it before migrations/shared/0069_project_create_permissions.sql,
// which had to land with the route: gating on a permission nothing grants is
// 403-for-everyone.
const CreateProjectPermission = "projects.projects.project.create"

// administrationMode is the only `{mode}` this route answers on.
//
// pylon registers create on its AdminAPI alone — `mode_handlers` maps
// `administration` to AdminAPI and `default` to ProjectAPI, and ProjectAPI has
// no `post`, so `POST /projects/project/default` is a 404 there. Reproduced
// here rather than left to the permission gate, so that the wrong mode is "no
// such route" instead of "you may not", which is what a client sees today.
const administrationMode = "administration"

// createProjectRequest is legacy/plugins/projects/models/pd/project.py's
// ProjectCreatePD.
//
// Every limit is a pointer so that an omitted field takes ProjectCreatePD's
// default rather than a Go zero value — the difference between "unlimited" (-1)
// and "zero" for cpu_limit, and between 5000 and 0 for the VCU ceiling.
type createProjectRequest struct {
	Name string `json:"name"`
	// Accepts a bare string or a list, as ProjectCreatePD's
	// `Optional[Union[str, List[str]]]` does. See UnmarshalJSON on adminEmails.
	ProjectAdminEmail adminEmails `json:"project_admin_email"`
	Plugins           []string    `json:"plugins"`

	DataRetentionLimit     *int32 `json:"data_retention_limit"`
	TestDurationLimit      *int32 `json:"test_duration_limit"`
	CPULimit               *int32 `json:"cpu_limit"`
	MemoryLimit            *int32 `json:"memory_limit"`
	VCUHardLimit           *int32 `json:"vcu_hard_limit"`
	VCUSoftLimit           *int32 `json:"vcu_soft_limit"`
	VCULimitTotalBlock     *bool  `json:"vcu_limit_total_block"`
	StorageHardLimit       *int32 `json:"storage_hard_limit"`
	StorageSoftLimit       *int32 `json:"storage_soft_limit"`
	StorageLimitTotalBlock *bool  `json:"storage_limit_total_block"`
}

// adminEmails accepts either JSON shape ProjectCreatePD allows.
type adminEmails []string

func (a *adminEmails) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		*a = nil
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var list []string
		if err := json.Unmarshal(data, &list); err != nil {
			return err
		}
		*a = list
		return nil
	}
	var single string
	if err := json.Unmarshal(data, &single); err != nil {
		return err
	}
	if single == "" {
		*a = nil
		return nil
	}
	*a = []string{single}
	return nil
}

// createProjectResponse is AdminAPI.post's body.
//
// `id` is omitted on the failure branch, as it is there — the reference only
// adds the key when the status is 201 and a project row survived.
type createProjectResponse struct {
	Steps         []projectprovisioning.StepStatus `json:"steps"`
	RollbackSteps []projectprovisioning.StepStatus `json:"rollback_steps"`
	ID            *int64                           `json:"id,omitempty"`
}

// CreateProject serves `POST /api/v2/projects/project/{mode}`.
func (h *Handler) CreateProject(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "mode") != administrationMode {
		apierr.WriteStatus(w, http.StatusNotFound, "not found")
		return
	}
	if h.provisioner == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}

	// The owner is the authenticated caller, never a body field — pylon reads
	// `g.auth.id`. The permission middleware has already resolved and rewritten
	// the user id on the context by the time this runs.
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ownerID, ok := user.OwningUserID()
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var body createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		// ProjectCreatePD declares `name: constr(min_length=1)`, so an empty
		// name is a validation failure there too.
		apierr.WriteStatus(w, http.StatusBadRequest, "name is required")
		return
	}

	request := projectprovisioning.Request{
		Name:        body.Name,
		Plugins:     body.Plugins,
		OwnerID:     ownerID,
		AdminEmails: body.ProjectAdminEmail,
		// Hardcoded, exactly as `context = {..., 'roles': ['admin', ]}` is in
		// the reference. It is not a client-supplied field there or here: a
		// caller choosing the role it grants would be a privilege decision made
		// in a request body.
		AdminRoles: []string{"admin"},
		Limits:     body.limits(),
	}

	result, err := h.provisioner.Provision(r.Context(), request)
	if err != nil {
		// The reference answers 400 for every provisioning failure, including
		// the ones that are plainly internal, and carries the per-step progress
		// so the caller can see how far it got. The status is preserved; the
		// step messages are already caller-safe (see StepStatus.setFailed).
		status := http.StatusBadRequest
		if !errors.Is(err, projectprovisioning.ErrUnknownAdminEmail) &&
			!errors.Is(err, projectprovisioning.ErrNameRequired) &&
			!errors.Is(err, projectprovisioning.ErrOwnerRequired) {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, createProjectResponse{
			Steps:         nonNilSteps(result.Steps),
			RollbackSteps: nonNilSteps(result.RollbackSteps),
		})
		return
	}

	projectID := result.ProjectID
	writeJSON(w, http.StatusCreated, createProjectResponse{
		Steps:         nonNilSteps(result.Steps),
		RollbackSteps: nonNilSteps(result.RollbackSteps),
		ID:            &projectID,
	})
}

// DeleteProjectPermission gates the delete route. Same provenance and the same
// administration-mode-only grant as CreateProjectPermission.
const DeleteProjectPermission = "projects.projects.project.delete"

// deleteProjectResponse is `delete_project`'s body: one status per step, under
// the key `steps`.
type deleteProjectResponse struct {
	Steps []projectprovisioning.StepStatus `json:"steps"`
	// Pending and Message appear with a 202: the cleanup steps still to finish,
	// and what happens to them. Message also carries the reason of a 500.
	Pending []string `json:"pending,omitempty"`
	Message string   `json:"message,omitempty"`
	// Database names the PgVector database a delete has not dropped yet, when
	// the project was recorded as having one (#1211).
	Database string `json:"database,omitempty"`
}

// DeleteProject serves `DELETE /api/v2/projects/project/{mode}/{projectID}`.
//
// Destructive and irreversible: it removes the project row and revokes its
// credentials in one transaction, then cleans up what is slow or external
// (artifact bytes, the tenant schema with CASCADE, the PgVector database) from
// the cleanup journal. It is gated on the same administration-mode permission
// the reference declares, and answers 404 for any other `{mode}`.
//
// The answers:
//
//   - 200: the project is gone and every cleanup step finished within the
//     request's budget.
//   - 202: the project is gone and its credentials are revoked, but some cleanup
//     steps are still pending or failed. The body lists them under `pending`;
//     the cleanup journal and its reconciler finish them in the background. A
//     retry of the delete answers 404.
//   - 404: no such project (also a repeated delete).
//   - 409: the project has active runs; nothing changed.
//   - 500: the delete did not happen; the project is unchanged.
func (h *Handler) DeleteProject(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "mode") != administrationMode {
		apierr.WriteStatus(w, http.StatusNotFound, "not found")
		return
	}
	if h.provisioner == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || projectID <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid project id")
		return
	}

	budget := h.deleteBudget
	if budget <= 0 {
		budget = DefaultDeleteBudget
	}
	result, err := h.provisioner.Deprovision(r.Context(), projectID, projectprovisioning.WithCleanupBudget(budget))
	switch {
	case errors.Is(err, projectprovisioning.ErrProjectNotFound):
		apierr.WriteStatus(w, http.StatusNotFound, "project not found")
		return
	case errors.Is(err, projectprovisioning.ErrProjectWorkActive):
		// The deciding transaction's refusal, and only that: it rolls back
		// before anything changes, so "retry later" is true. A cleanup leftover
		// can never be this.
		apierr.WriteStatus(w, http.StatusConflict,
			"project has active runs; stop them or wait for them to finish, then retry the delete")
		return
	case errors.Is(err, projectprovisioning.ErrProjectNotRemoved), err != nil && result.ProjectID == 0:
		// The reference answers 200 even when every step failed. Reporting a
		// project that still exists as deleted is the failure mode this route
		// exists to avoid, so the per-step detail is returned with a 500.
		writeJSON(w, http.StatusInternalServerError, deleteProjectResponse{
			Steps:   nonNilSteps(result.RollbackSteps),
			Message: "the project was not removed and is unchanged; retry the delete",
		})
		return
	case err != nil || len(result.Pending) > 0:
		// The project row and every credential are gone; only cleanup is left.
		// That is accepted work, not a failure and not a finished delete: the
		// journal retries it with backoff. The body names what is pending, never
		// the underlying error, which can hold SQL or addresses.
		writeJSON(w, http.StatusAccepted, deleteProjectResponse{
			Steps:    nonNilSteps(result.RollbackSteps),
			Pending:  result.Pending,
			Message:  "the project was deleted and its access revoked; cleanup of the remaining steps continues in the background",
			Database: result.VectorDatabase,
		})
		return
	}
	writeJSON(w, http.StatusOK, deleteProjectResponse{Steps: nonNilSteps(result.RollbackSteps)})
}

// limits applies ProjectCreatePD's defaults to the fields the body omitted.
func (b createProjectRequest) limits() projectprovisioning.Limits {
	limits := projectprovisioning.DefaultLimits()
	if b.DataRetentionLimit != nil {
		limits.DataRetentionLimit = *b.DataRetentionLimit
	}
	if b.TestDurationLimit != nil {
		limits.TestDurationLimit = *b.TestDurationLimit
	}
	if b.CPULimit != nil {
		limits.CPULimit = *b.CPULimit
	}
	if b.MemoryLimit != nil {
		limits.MemoryLimit = *b.MemoryLimit
	}
	if b.VCUHardLimit != nil {
		limits.VCUHardLimit = *b.VCUHardLimit
	}
	if b.VCUSoftLimit != nil {
		limits.VCUSoftLimit = *b.VCUSoftLimit
	}
	if b.VCULimitTotalBlock != nil {
		limits.VCULimitTotalBlock = *b.VCULimitTotalBlock
	}
	if b.StorageHardLimit != nil {
		limits.StorageHardLimit = *b.StorageHardLimit
	}
	if b.StorageSoftLimit != nil {
		limits.StorageSoftLimit = *b.StorageSoftLimit
	}
	if b.StorageLimitTotalBlock != nil {
		limits.StorageLimitTotalBlock = *b.StorageLimitTotalBlock
	}
	return limits
}

// nonNilSteps keeps the two arrays as `[]` rather than `null` in JSON, which is
// the shape the reference emits for an empty list.
func nonNilSteps(steps []projectprovisioning.StepStatus) []projectprovisioning.StepStatus {
	if steps == nil {
		return []projectprovisioning.StepStatus{}
	}
	return steps
}
