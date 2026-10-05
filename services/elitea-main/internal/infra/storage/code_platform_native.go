package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/codeplatform"
	toolkit "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type CodeApplicationCatalog interface {
	List(context.Context, applications.ListRequest) (applications.ListResponse, error)
	Get(context.Context, string, string) (applications.Application, error)
	GetVersion(context.Context, string, string, string) (applications.Version, error)
}

// The existing v2 toolkit repository satisfies this narrow projection interface.
type CodeToolkitCatalog interface {
	ListToolkits(context.Context, string, int, int) ([]map[string]any, int, error)
}

// CodeNativeReader is constructed after original-visit/retained-runtime admission.
// No Code request can replace its claim, job, actor, project, or dependencies.
type CodeNativeReader struct {
	claim        ContentClaim
	admission    domain.Admission
	authorizer   AgentRuntimeContextAuthorizer
	permissions  auth.PermissionResolver
	secrets      *RuntimeCodeSecretService
	applications CodeApplicationCatalog
	toolkits     CodeToolkitCatalog
	runs         *toolkit.RunService
}

func NewCodeNativeReader(claim ContentClaim, a domain.Admission, authorizer AgentRuntimeContextAuthorizer, permissions auth.PermissionResolver, secrets *RuntimeCodeSecretService, applications CodeApplicationCatalog, toolkits CodeToolkitCatalog, runs *toolkit.RunService) (*CodeNativeReader, error) {
	if a.Validate() != nil || claim.PeerCertificate == nil || claim.ExecutionID != a.Job.ExecutionID || claim.ClaimID != a.ClaimID || claim.Generation != a.Generation || !bytes.Equal(claim.FenceToken, a.FenceToken) || authorizer == nil || permissions == nil || secrets == nil || applications == nil || toolkits == nil || runs == nil {
		return nil, domain.ErrUnauthorized
	}
	claim.FenceToken = append([]byte(nil), claim.FenceToken...)
	a.FenceToken = append([]byte(nil), a.FenceToken...)
	return &CodeNativeReader{claim: claim, admission: a, authorizer: authorizer, permissions: permissions, secrets: secrets, applications: applications, toolkits: toolkits, runs: runs}, nil
}
func (r *CodeNativeReader) matches(a domain.Admission) bool {
	b := r.admission
	return a.Validate() == nil && a.Job == b.Job && a.ClaimID == b.ClaimID && a.Generation == b.Generation && a.LeaseEpoch == b.LeaseEpoch && a.WorkloadIdentity == b.WorkloadIdentity && bytes.Equal(a.FenceToken, b.FenceToken)
}
func status(code string) app.Outcome {
	return app.Outcome{Known: true, Status: code, Result: json.RawMessage("null")}
}
func (r *CodeNativeReader) AuthorizeCodeSecretRead(ctx context.Context, a domain.Admission, scope, name string) error {
	if r == nil || !r.matches(a) {
		return domain.ErrUnauthorized
	}
	return r.secrets.Authorize(ctx, r.claim, scope, name)
}
func (r *CodeNativeReader) ReadCodeSecret(ctx context.Context, a domain.Admission, scope, name string) (app.Outcome, error) {
	if r == nil || !r.matches(a) {
		return app.Outcome{}, domain.ErrUnauthorized
	}
	value, err := r.secrets.Read(ctx, r.claim, scope, name)
	switch {
	case err == nil:
		return catalogResult(value)
	case errors.Is(err, ErrCodeSecretNotFound):
		return status("not_found"), nil
	case errors.Is(err, ErrCodeSecretSharingDenied):
		return status("sharing_denied"), nil
	case errors.Is(err, ErrCodeSecretPolicyDenied), errors.Is(err, ErrContentUnauthorized), errors.Is(err, auth.ErrPermissionDenied):
		return status("authorization_denied"), nil
	case errors.Is(err, ErrContentRejected):
		return status("invalid_resource"), nil
	default:
		return status("dependency_unavailable"), nil
	}
}
func (r *CodeNativeReader) AuthorizeCodeToolkitCall(ctx context.Context, a domain.Admission) error {
	_, err := r.authorize(ctx, a, "models.applications.tool.patch")
	if errors.Is(err, auth.ErrPermissionDenied) || errors.Is(err, ErrContentUnauthorized) {
		return domain.ErrUnauthorized
	}
	return err
}
func (r *CodeNativeReader) authorize(ctx context.Context, a domain.Admission, permission string) (context.Context, error) {
	if r == nil || ctx == nil || !r.matches(a) {
		return ctx, domain.ErrUnauthorized
	}
	principal, err := r.authorizer.AuthorizeAgentRuntimeContext(ctx, r.claim)
	if err != nil {
		return ctx, err
	}
	actor := strconv.FormatInt(a.Job.ActorID, 10)
	if principal.ActorID != actor || principal.ResourceProjectID != a.Job.ProjectID {
		return ctx, domain.ErrUnauthorized
	}
	user := auth.User{ID: actor, UserID: actor}
	resolved, err := r.permissions.ResolvePermissions(ctx, user, auth.PermissionModeDefault, strconv.FormatInt(a.Job.ProjectID, 10))
	if err != nil {
		return ctx, err
	}
	if resolved.UserID != a.Job.ActorID {
		return ctx, domain.ErrUnauthorized
	}
	if permission != "" {
		allowed := false
		for _, candidate := range resolved.Permissions {
			if candidate == permission {
				allowed = true
				break
			}
		}
		if !allowed {
			return ctx, auth.ErrPermissionDenied
		}
	}
	return auth.ContextWithUser(ctx, user), nil
}
func (r *CodeNativeReader) ReadCodeCatalog(ctx context.Context, a domain.Admission, request domain.Request) (app.Outcome, error) {
	permission := ""
	switch request.Operation {
	case "application_list":
		permission = "models.applications.applications.list"
	case "application_get":
		permission = "models.applications.application.details"
	case "application_version_get":
		permission = "models.applications.version.details"
	case "toolkit_list":
		permission = "models.applications.tools.list"
	case "user_get":
	default:
		return status("unsupported_operation"), nil
	}
	ctx, err := r.authorize(ctx, a, permission)
	if err != nil {
		return nativeReadFailure(err), nil
	}
	project := strconv.FormatInt(a.Job.ProjectID, 10)
	switch request.Operation {
	case "user_get":
		return catalogResult(map[string]string{"id": strconv.FormatInt(a.Job.ActorID, 10)})
	case "application_get", "application_version_get":
		id := request.Resource["id"]
		if request.Operation == "application_version_get" {
			id = request.Resource["application_id"]
		}
		numeric, err := strconv.ParseInt(id, 10, 32)
		if err != nil || numeric < 1 {
			return status("invalid_resource"), nil
		}
		// The existing folder-visible list checks the executing actor before Get.
		visible, err := r.applications.List(ctx, applications.ListRequest{ProjectID: project, Page: 1, PageSize: 1, IDs: []int32{int32(numeric)}})
		if err != nil {
			return nativeReadFailure(err), nil
		}
		if len(visible.Rows) != 1 || visible.Rows[0].ID != id {
			return status("not_found"), nil
		}
		if request.Operation == "application_get" {
			value, err := r.applications.Get(ctx, project, id)
			if err != nil {
				return nativeReadFailure(err), nil
			}
			return catalogResult(value)
		}
		value, err := r.applications.GetVersion(ctx, project, id, request.Resource["version_id"])
		if err != nil {
			return nativeReadFailure(err), nil
		}
		return catalogResult(value)
	case "application_list", "toolkit_list":
		page, limit, err := catalogPage(a.Job, request)
		if err != nil {
			return status("invalid_resource"), nil
		}
		if request.Operation == "application_list" {
			value, err := r.applications.List(ctx, applications.ListRequest{ProjectID: project, Page: page, PageSize: limit})
			if err != nil {
				return nativeReadFailure(err), nil
			}
			var next any
			if page*limit < value.Total && page < int(a.Job.MaxCalls) {
				next = catalogCursor(a.Job, request.Operation, limit, page+1)
			}
			return catalogResult(map[string]any{"rows": value.Rows, "next_cursor": next})
		}
		rows, total, err := r.toolkits.ListToolkits(ctx, project, page, limit)
		if err != nil {
			return nativeReadFailure(err), nil
		}
		if len(rows) > limit {
			return status("dependency_unavailable"), nil
		}
		projected := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			id, ok := row["id"].(string)
			if !ok {
				return status("dependency_unavailable"), nil
			}
			numeric, err := strconv.ParseInt(id, 10, 32)
			if err != nil || numeric < 1 {
				return status("dependency_unavailable"), nil
			}
			// The native resolver independently applies exact ID/actor/folder visibility.
			revision, err := r.runs.ResolveCodeToolkitRevision(ctx, toolkit.RunRequest{ProjectID: a.Job.ProjectID, ActorUserID: a.Job.ActorID, ToolkitID: numeric, ToolName: "catalog_metadata", Arguments: json.RawMessage("{}")})
			if errors.Is(err, toolkit.ErrToolkitNotVisible) {
				continue
			}
			if err != nil {
				return nativeReadFailure(err), nil
			}
			entry := map[string]any{"id": id, "revision": hex.EncodeToString(revision[:])}
			for _, field := range []string{"type", "name", "description"} {
				value, ok := row[field].(string)
				if !ok || len(value) > 4096 {
					return status("dependency_unavailable"), nil
				}
				entry[field] = value
			}
			projected = append(projected, entry)
		}
		var next any
		if page*limit < total && page < int(a.Job.MaxCalls) {
			next = catalogCursor(a.Job, request.Operation, limit, page+1)
		}
		return catalogResult(map[string]any{"rows": projected, "next_cursor": next})
	}
	return status("unsupported_operation"), nil
}
func nativeReadFailure(err error) app.Outcome {
	if errors.Is(err, domain.ErrUnauthorized) || errors.Is(err, ErrContentUnauthorized) || errors.Is(err, auth.ErrPermissionDenied) {
		return status("authorization_denied")
	}
	var api *apierr.APIError
	if errors.As(err, &api) && api.Status == 404 {
		return status("not_found")
	}
	return status("dependency_unavailable")
}
func catalogCursor(job domain.Job, operation string, limit, page int) string {
	hash := sha256.New()
	hash.Write([]byte("elitea.code.catalog-page.v1\x00"))
	hash.Write(job.PreparedRequest[:])
	hash.Write(job.Activation[:])
	hash.Write([]byte(operation))
	hash.Write([]byte{0})
	hash.Write([]byte(strconv.Itoa(limit)))
	hash.Write([]byte{0})
	hash.Write([]byte(strconv.Itoa(page)))
	return hex.EncodeToString(hash.Sum(nil))
}
func catalogPage(job domain.Job, request domain.Request) (int, int, error) {
	var args struct {
		Cursor *string `json:"cursor"`
		Limit  int     `json:"limit"`
	}
	if json.Unmarshal(request.Arguments, &args) != nil || args.Limit < 1 || args.Limit > 100 {
		return 0, 0, domain.ErrConflict
	}
	if args.Cursor == nil {
		return 1, args.Limit, nil
	}
	// MaxCalls bounds both the number of pages and this fixed CPU lookup.
	for page := 2; page <= int(job.MaxCalls); page++ {
		if catalogCursor(job, request.Operation, args.Limit, page) == *args.Cursor {
			return page, args.Limit, nil
		}
	}
	return 0, 0, domain.ErrConflict
}

type codeCatalogBytes []byte

func (w *codeCatalogBytes) Write(value []byte) (int, error) {
	if len(value) > domain.MaxReplyHeader-len(*w) {
		return 0, io.ErrShortBuffer
	}
	*w = append(*w, value...)
	return len(value), nil
}
func catalogResult(value any) (app.Outcome, error) {
	var output codeCatalogBytes
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return status("resource_exhausted"), nil
	}
	return app.Outcome{Known: true, Status: "ok", Result: json.RawMessage(bytes.TrimSuffix(output, []byte{'\n'}))}, nil
}

// AuthorizeCodeReply checks only current permission and visibility. It never
// calls RunTool or reads a secret again. Catalog pages are bounded to 100 rows.
func (r *CodeNativeReader) AuthorizeCodeReply(ctx context.Context, a domain.Admission, request domain.Request, reply domain.Reply) error {
	permission := ""
	switch request.Operation {
	case "application_list":
		permission = "models.applications.applications.list"
	case "application_get":
		permission = "models.applications.application.details"
	case "application_version_get":
		permission = "models.applications.version.details"
	case "toolkit_list":
		permission = "models.applications.tools.list"
	case "user_get":
	default:
		return domain.ErrUnauthorized
	}
	ctx, err := r.authorize(ctx, a, permission)
	if err != nil {
		return err
	}
	project := strconv.FormatInt(a.Job.ProjectID, 10)
	if request.Operation == "user_get" || reply.Status == "not_found" {
		return nil
	}
	if request.Operation == "application_get" || request.Operation == "application_version_get" {
		id := request.Resource["id"]
		if request.Operation == "application_version_get" {
			id = request.Resource["application_id"]
		}
		return r.authorizeCodeApplications(ctx, project, []string{id})
	}
	var page struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}
	if json.Unmarshal(reply.Result, &page) != nil || len(page.Rows) > 100 {
		return domain.ErrConflict
	}
	ids := make([]string, 0, len(page.Rows))
	for _, row := range page.Rows {
		var id string
		if json.Unmarshal(row["id"], &id) != nil {
			return domain.ErrConflict
		}
		if request.Operation == "application_list" {
			ids = append(ids, id)
			continue
		}
		numeric, err := strconv.ParseInt(id, 10, 32)
		if err != nil || numeric < 1 {
			return domain.ErrConflict
		}
		var selected string
		if json.Unmarshal(row["revision"], &selected) != nil {
			return domain.ErrConflict
		}
		revision, err := r.runs.ResolveCodeToolkitRevision(ctx, toolkit.RunRequest{ProjectID: a.Job.ProjectID, ActorUserID: a.Job.ActorID, ToolkitID: numeric, ToolName: "catalog_metadata", Arguments: json.RawMessage("{}")})
		if err != nil {
			return err
		}
		if hex.EncodeToString(revision[:]) != selected {
			return domain.ErrUnauthorized
		}
	}
	if request.Operation == "application_list" {
		return r.authorizeCodeApplications(ctx, project, ids)
	}
	return nil
}
func (r *CodeNativeReader) authorizeCodeApplications(ctx context.Context, project string, ids []string) error {
	if len(ids) > 100 {
		return domain.ErrConflict
	}
	if len(ids) == 0 {
		return nil
	}
	selected := make([]int32, 0, len(ids))
	expected := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		numeric, err := strconv.ParseInt(id, 10, 32)
		if err != nil || numeric < 1 || strconv.FormatInt(numeric, 10) != id {
			return domain.ErrConflict
		}
		if _, found := expected[id]; found {
			return domain.ErrConflict
		}
		expected[id] = struct{}{}
		selected = append(selected, int32(numeric))
	}
	visible, err := r.applications.List(ctx, applications.ListRequest{ProjectID: project, Page: 1, PageSize: len(selected), IDs: selected})
	if err != nil {
		return err
	}
	if len(visible.Rows) != len(expected) {
		return domain.ErrUnauthorized
	}
	for _, row := range visible.Rows {
		if _, found := expected[row.ID]; !found {
			return domain.ErrUnauthorized
		}
		delete(expected, row.ID)
	}
	if len(expected) != 0 {
		return domain.ErrUnauthorized
	}
	return nil
}
