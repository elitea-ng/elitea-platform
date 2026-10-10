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
	// Message and Database appear only on the failures an operator acts on:
	// Database names the PgVector database a delete left behind (#1211).
	Message  string `json:"message,omitempty"`
	Database string `json:"database,omitempty"`
}

// DeleteProject serves `DELETE /api/v2/projects/project/{mode}/{projectID}`.
//
// Destructive and irreversible: it drops the tenant schema with CASCADE. It is
// gated on the same administration-mode permission the reference declares, and
// answers 404 for any other `{mode}`.
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

	result, err := h.provisioner.Deprovision(r.Context(), projectID)
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
	case err != nil:
		// The reference answers 200 even when every step failed. Reporting a
		// project that still exists as deleted is the failure mode this route
		// exists to avoid, so the per-step detail is returned with a 500. The
		// message names EVERY leftover the joined error carries: a retry answers
		// 404 once the row is gone, so whatever this omits is only in the
		// cleanup journal.
		writeJSON(w, http.StatusInternalServerError, deleteProjectResponse{
			Steps:    nonNilSteps(result.RollbackSteps),
			Message:  deleteFailureMessage(err, result.RollbackSteps),
			Database: result.VectorDatabase,
		})
		return
	}
	writeJSON(w, http.StatusOK, deleteProjectResponse{Steps: nonNilSteps(result.RollbackSteps)})
}

// deleteFailureMessage renders one line per leftover a failed delete reports.
// The text is fixed per leftover and never carries the underlying error, which
// can hold SQL or addresses.
func deleteFailureMessage(err error, steps []projectprovisioning.StepStatus) string {
	var lines []string
	if errors.Is(err, projectprovisioning.ErrProjectNotRemoved) {
		lines = append(lines, "the project was not removed and is unchanged; retry the delete")
	}
	if errors.Is(err, projectprovisioning.ErrArtifactsNotRemoved) {
		lines = append(lines, "the project was deleted, but its artifact buckets were not purged yet; the cleanup journal retries the purge")
	}
	if errors.Is(err, projectprovisioning.ErrTenantSchemaNotRemoved) {
		lines = append(lines, "the project was deleted, but its tenant schema was not removed yet; the cleanup journal retries it")
	}
	if errors.Is(err, projectprovisioning.ErrVectorStoreNotDropped) {
		lines = append(lines, "the project was deleted, but its PgVector database was not dropped yet; the cleanup journal retries it")
	}
	if errors.Is(err, projectprovisioning.ErrCleanupIncomplete) {
		for _, status := range steps {
			switch status.Step {
			case projectprovisioning.StepArtifactBuckets, projectprovisioning.StepProjectSchema,
				projectprovisioning.StepProjectPgvectorDrop, projectprovisioning.StepProjectModel:
				continue
			}
			if status.OK != nil && !*status.OK {
				lines = append(lines, "the project was deleted, but step "+status.Step+" did not complete; the cleanup journal retries it")
			}
		}
	}
	if len(lines) == 0 {
		return "project delete did not complete; see the steps"
	}
	return strings.Join(lines, "\n")
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
