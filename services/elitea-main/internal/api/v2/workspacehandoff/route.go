// Package workspacehandoff serves the desktop and web routes of the desktop ⇄
// cloud handoff (ADR-0034 decision 14, wire contract §2): the handoff state
// machine (internal/application/workspacehandoff) and the workspace store
// (internal/application/workspacestore). The routes are thin: they
// authenticate, authorize, decode and map errors; the later worker gRPC calls
// adapt the same application methods.
package workspacehandoff

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/desktopwire"
	handoffapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The route patterns (contract §2).
const (
	HandoffPath        = "/api/v2/elitea_core/workspace_handoff/prompt_lib/{projectID}/{conversationUUID}"
	CompletePath       = "/api/v2/elitea_core/workspace_handoff_complete/prompt_lib/{projectID}/{handoffID}"
	ReclaimPath        = "/api/v2/elitea_core/workspace_handoff_reclaim/prompt_lib/{projectID}/{conversationUUID}"
	ReclaimRequestPath = "/api/v2/elitea_core/workspace_handoff_reclaim_request/prompt_lib/{projectID}/{conversationUUID}"
	ReleasePath        = "/api/v2/elitea_core/workspace_handoff_release/prompt_lib/{projectID}/{conversationUUID}"
	UploadPath         = "/api/v2/elitea_core/workspace_store_upload/prompt_lib/{projectID}/{conversationUUID}"
	UploadCompletePath = "/api/v2/elitea_core/workspace_store_upload_complete/prompt_lib/{projectID}/{grantID}"
	PublishPath        = "/api/v2/elitea_core/workspace_store_publish/prompt_lib/{projectID}/{conversationUUID}"
	FetchPath          = "/api/v2/elitea_core/workspace_store_fetch/prompt_lib/{projectID}/{conversationUUID}"
	ObjectPath         = "/api/v2/elitea_core/workspace_store_object/prompt_lib/{projectID}/{conversationUUID}/{sha256}"
	PolicyPath         = "/api/v2/elitea_core/workspace_handoff_policy/prompt_lib/{projectID}"

	// SendPermission is the chat send's: every handoff write moves the
	// conversation. 0068 grants it, so no migration seeds anything.
	SendPermission = "models.chat.messages.create"
	// ReadPermission reads the conversation details (the banner state).
	ReadPermission = "models.chat.conversation.details"
	// ManagePermission lets a project admin erase another user's cloud copy.
	ManagePermission = "models.project_settings.edit"
	Mode             = auth.PermissionModeDefault

	// maxBody bounds one JSON body: a send carries its exclusion list, its
	// includes and up to 64 KiB of environment.
	maxBody = int64(2 * 1024 * 1024)
)

// Methods lists every (method, pattern) the route serves, for the router.
var Methods = []struct{ Method, Pattern string }{
	{http.MethodPost, HandoffPath}, {http.MethodGet, HandoffPath}, {http.MethodDelete, HandoffPath},
	{http.MethodPost, CompletePath}, {http.MethodPost, ReclaimPath}, {http.MethodPost, ReclaimRequestPath},
	{http.MethodPost, ReleasePath}, {http.MethodPost, UploadPath}, {http.MethodPost, UploadCompletePath},
	{http.MethodPost, PublishPath}, {http.MethodGet, FetchPath},
	{http.MethodPut, ObjectPath}, {http.MethodGet, ObjectPath},
	{http.MethodGet, PolicyPath},
}

var ErrInvalidRoute = errors.New("invalid workspace handoff route dependencies")

// Handoff is *handoffapp.Service.
type Handoff interface {
	Begin(context.Context, handoffapp.BeginRequest) (handoffapp.BeginOutcome, error)
	Complete(context.Context, handoffapp.CompleteRequest) (handoffapp.Handoff, error)
	Reclaim(context.Context, handoffapp.ReclaimRequest) (handoffapp.Handoff, error)
	RequestReclaim(context.Context, handoffapp.Thread, int64) error
	Release(context.Context, handoffapp.ReleaseRequest) (handoffapp.Handoff, error)
	State(context.Context, handoffapp.Thread) (handoffapp.View, error)
	Erase(context.Context, handoffapp.EraseRequest) (handoffapp.Handoff, error)
	EffectivePolicy(context.Context, int64) (handoffapp.Policy, error)
}

// Store is *workspacestore.Service.
type Store interface {
	RequestUpload(context.Context, workspacestore.UploadRequest) (workspacestore.UploadGrant, error)
	CompleteUpload(context.Context, int64, workspacestore.Caller, string, string, []workspacestore.CompletedPart) error
	Publish(context.Context, workspacestore.PublishRequest) (workspacestore.PublishResult, error)
	Plan(context.Context, workspacestore.Thread, workspacestore.Caller, int64) (workspacestore.FetchPlan, error)
	PutObject(context.Context, workspacestore.Thread, workspacestore.Caller, string, io.Reader) error
	OpenObject(context.Context, workspacestore.Thread, workspacestore.Caller, string) (io.ReadCloser, int64, error)
	ThreadBytes(context.Context, workspacestore.Thread) (int64, error)
	ProjectBytes(context.Context, int64) (int64, error)
}

// ConversationReader authorizes reading a conversation
// (repos.ConversationsRepo.AuthorizeChatResource).
type ConversationReader interface {
	AuthorizeChatResource(ctx context.Context, projectID, kind, resourceID string) error
}

var (
	_ Handoff = (*handoffapp.Service)(nil)
	_ Store   = (*workspacestore.Service)(nil)
)

// Route serves every handoff and store route behind Auth and the project
// permission.
type Route struct {
	handler http.Handler
}

func NewRoute(handoff Handoff, store Store, conversations ConversationReader, authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver) (*Route, error) {
	if handoff == nil || store == nil || conversations == nil || authConfig.PrincipalValidator == nil || permissions == nil {
		return nil, ErrInvalidRoute
	}
	h := &handler{handoff: handoff, store: store, conversations: conversations}
	gate := func(endpoint http.HandlerFunc, required ...string) http.Handler {
		wrapped := apimw.RequireResolvedPermissionsForProject(
			permissions, Mode,
			func(request *http.Request) (string, bool) {
				projectID := chi.URLParam(request, "projectID")
				_, valid := desktopwire.PositiveID(projectID)
				return projectID, valid
			},
			required...,
		)(endpoint)
		return apimw.Auth(authConfig)(wrapped)
	}
	router := chi.NewRouter()
	router.Method(http.MethodPost, HandoffPath, gate(h.begin, SendPermission))
	router.Method(http.MethodGet, HandoffPath, gate(h.state, ReadPermission))
	router.Method(http.MethodDelete, HandoffPath, gate(h.erase, SendPermission, ManagePermission))
	router.Method(http.MethodPost, CompletePath, gate(h.complete, SendPermission))
	router.Method(http.MethodPost, ReclaimPath, gate(h.reclaim, SendPermission))
	router.Method(http.MethodPost, ReclaimRequestPath, gate(h.reclaimRequest, SendPermission))
	router.Method(http.MethodPost, ReleasePath, gate(h.release, SendPermission))
	router.Method(http.MethodPost, UploadPath, gate(h.upload, SendPermission))
	router.Method(http.MethodPost, UploadCompletePath, gate(h.uploadComplete, SendPermission))
	router.Method(http.MethodPost, PublishPath, gate(h.publish, SendPermission))
	router.Method(http.MethodGet, FetchPath, gate(h.fetch, SendPermission))
	router.Method(http.MethodPut, ObjectPath, gate(h.putObject, SendPermission))
	router.Method(http.MethodGet, ObjectPath, gate(h.getObject, SendPermission))
	router.Method(http.MethodGet, PolicyPath, gate(h.policy, SendPermission))
	return &Route{handler: router}, nil
}

func (route *Route) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type handler struct {
	handoff       Handoff
	store         Store
	conversations ConversationReader
}

// caller answers the authenticated owning user. nativeOnly refuses every
// credential but a native app access token (contract §2: the desktop routes).
func caller(writer http.ResponseWriter, request *http.Request, nativeOnly bool) (auth.User, int64, bool) {
	user, ok := auth.UserFromContext(request.Context())
	if !ok {
		desktopwire.WriteError(writer, http.StatusUnauthorized, "unauthorized", "authentication required")
		return auth.User{}, 0, false
	}
	actor, ok := user.OwningUserID()
	if !ok {
		desktopwire.WriteError(writer, http.StatusUnauthorized, "unauthorized", "authentication required")
		return auth.User{}, 0, false
	}
	if nativeOnly && (user.TokenID == "" || user.NativeClientID == "" || !strings.EqualFold(user.AuthType, "token")) {
		desktopwire.WriteError(writer, http.StatusForbidden, "handoff_requires_token",
			"This operation needs the Elitea desktop app's sign-in.")
		return auth.User{}, 0, false
	}
	return user, actor, true
}

func thread(writer http.ResponseWriter, request *http.Request) (workspacestore.Thread, bool) {
	projectID, ok := desktopwire.PositiveID(chi.URLParam(request, "projectID"))
	conversation := chi.URLParam(request, "conversationUUID")
	if !ok || !workspacestore.ValidUUID(conversation) {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_handoff", "Invalid handoff request")
		return workspacestore.Thread{}, false
	}
	return workspacestore.Thread{ProjectID: projectID, ConversationUUID: conversation}, true
}

func decodeBody(writer http.ResponseWriter, request *http.Request, into any) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		desktopwire.WriteError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxBody)
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(into); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			desktopwire.WriteError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large")
			return false
		}
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_handoff", "Invalid handoff request")
		return false
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_handoff", "Invalid handoff request")
		return false
	}
	return true
}

func timestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func optionalTimestamp(value *time.Time) any {
	if value == nil {
		return nil
	}
	return timestamp(*value)
}

func optionalString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

type beginBody struct {
	DeviceID         string            `json:"device_id"`
	HistoryMode      string            `json:"history_mode"`
	Stats            handoffapp.Stats  `json:"stats"`
	HasUpstream      bool              `json:"has_upstream"`
	Note             string            `json:"note"`
	IncludedExcluded []string          `json:"included_excluded"`
	Env              map[string]string `json:"env"`
}

func (h *handler) begin(writer http.ResponseWriter, request *http.Request) {
	target, ok := thread(writer, request)
	if !ok {
		return
	}
	user, actor, ok := caller(writer, request, true)
	if !ok {
		return
	}
	var body beginBody
	if !decodeBody(writer, request, &body) {
		return
	}
	outcome, err := h.handoff.Begin(request.Context(), handoffapp.BeginRequest{
		Thread: target, ActorUserID: actor, TokenID: user.TokenID, DeviceID: body.DeviceID,
		HistoryMode: body.HistoryMode, Stats: body.Stats, HasUpstream: body.HasUpstream, Note: body.Note,
		AuditRoute: HandoffPath, IncludedExcluded: body.IncludedExcluded, Env: body.Env,
	})
	if err != nil {
		writeError(request.Context(), writer, "begin", err)
		return
	}
	expires := ""
	if outcome.Handoff.ExpiresAt != nil {
		expires = timestamp(*outcome.Handoff.ExpiresAt)
	}
	desktopwire.WriteJSON(writer, http.StatusCreated, map[string]any{
		"handoff_id":   outcome.Handoff.HandoffID,
		"epoch":        outcome.Handoff.Epoch,
		"state":        string(outcome.Handoff.State),
		"allowed_refs": outcome.AllowedRefs,
		"expected_old": outcome.ExpectedOld,
		"limits": map[string]any{
			"max_segment_bytes":            outcome.Limits.MaxSegmentBytes,
			"max_file_bytes":               outcome.Limits.MaxFileBytes,
			"max_files":                    outcome.Limits.MaxFiles,
			"max_unpacked_bytes":           outcome.Limits.MaxUnpackedBytes,
			"project_workspace_bytes_left": outcome.BytesLeft,
		},
		"expires_at": expires,
	})
}

func (h *handler) state(writer http.ResponseWriter, request *http.Request) {
	target, ok := thread(writer, request)
	if !ok {
		return
	}
	_, actor, ok := caller(writer, request, false)
	if !ok {
		return
	}
	// The banner is for whoever may read the conversation.
	if err := h.conversations.AuthorizeChatResource(request.Context(),
		strconv.FormatInt(target.ProjectID, 10), "conversation", target.ConversationUUID); err != nil {
		desktopwire.WriteError(writer, http.StatusNotFound, "handoff_not_found", "The conversation was not found.")
		return
	}
	view, err := h.handoff.State(request.Context(), target)
	if err != nil {
		writeError(request.Context(), writer, "state", err)
		return
	}
	if !view.Present {
		desktopwire.WriteJSON(writer, http.StatusOK, map[string]any{"state": "LOCAL", "epoch": 0})
		return
	}
	bytes, err := h.store.ThreadBytes(request.Context(), target)
	if err != nil {
		writeError(request.Context(), writer, "state", err)
		return
	}
	stats := statsFor(view.Handoff.Stats, actor == view.Handoff.OwnerUserID)
	desktopwire.WriteJSON(writer, http.StatusOK, map[string]any{
		"state":           string(view.Handoff.State),
		"epoch":           view.Handoff.Epoch,
		"reclaim_pending": view.Handoff.ReclaimPending,
		"handoff_id":      view.Handoff.HandoffID,
		"device_name":     view.Handoff.DeviceName,
		"history_mode":    view.Handoff.HistoryMode,
		"has_upstream":    view.Handoff.HasUpstream,
		"stats":           stats,
		"cloud_sha":       optionalString(view.Handoff.CloudSHA),
		"updated_at":      timestamp(view.Handoff.UpdatedAt),
		"delete_after":    optionalTimestamp(view.DeleteAfter),
		"workspace_bytes": bytes,
	})
}

// statsFor answers the send's stats for the state route. `excluded` lists
// paths on the owner's computer, so only the owner reads them; anyone else
// who may read the conversation gets `excluded_count` and no `excluded`.
// Everyone gets `excluded_count`.
func statsFor(raw []byte, owner bool) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	var stats map[string]any
	if err := json.Unmarshal(raw, &stats); err != nil {
		return json.RawMessage(`{}`)
	}
	excluded, _ := stats["excluded"].([]any)
	stats["excluded_count"] = len(excluded)
	if !owner {
		delete(stats, "excluded")
	}
	out, err := json.Marshal(stats)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}

func (h *handler) erase(writer http.ResponseWriter, request *http.Request) {
	target, ok := thread(writer, request)
	if !ok {
		return
	}
	_, actor, ok := caller(writer, request, false)
	if !ok {
		return
	}
	permissions, _ := apimw.ResolvedProjectPermissions(request.Context(), Mode, strconv.FormatInt(target.ProjectID, 10))
	handoff, err := h.handoff.Erase(request.Context(), handoffapp.EraseRequest{
		Thread: target, ActorUserID: actor, Manager: slices.Contains(permissions, ManagePermission),
		AuditRoute: HandoffPath,
	})
	if err != nil {
		writeError(request.Context(), writer, "erase", err)
		return
	}
	desktopwire.WriteJSON(writer, http.StatusOK, map[string]any{"state": "LOCAL", "epoch": handoff.Epoch})
}

type completeBody struct {
	Epoch      int64  `json:"epoch"`
	HandoffRef string `json:"handoff_ref"`
	SHA        string `json:"sha"`
}

func (h *handler) complete(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := desktopwire.PositiveID(chi.URLParam(request, "projectID"))
	if !ok {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_handoff", "Invalid handoff request")
		return
	}
	_, actor, ok := caller(writer, request, true)
	if !ok {
		return
	}
	var body completeBody
	if !decodeBody(writer, request, &body) {
		return
	}
	handoff, err := h.handoff.Complete(request.Context(), handoffapp.CompleteRequest{
		ProjectID: projectID, HandoffID: chi.URLParam(request, "handoffID"), ActorUserID: actor,
		Epoch: body.Epoch, HandoffRef: body.HandoffRef, SHA: body.SHA, AuditRoute: CompletePath,
	})
	if err != nil {
		writeError(request.Context(), writer, "complete", err)
		return
	}
	desktopwire.WriteJSON(writer, http.StatusOK, map[string]any{"state": string(handoff.State), "epoch": handoff.Epoch})
}

type reclaimBody struct {
	Epoch    int64  `json:"epoch"`
	DeviceID string `json:"device_id"`
}

func (h *handler) reclaim(writer http.ResponseWriter, request *http.Request) {
	target, ok := thread(writer, request)
	if !ok {
		return
	}
	_, actor, ok := caller(writer, request, true)
	if !ok {
		return
	}
	var body reclaimBody
	if !decodeBody(writer, request, &body) {
		return
	}
	handoff, err := h.handoff.Reclaim(request.Context(), handoffapp.ReclaimRequest{
		Thread: target, ActorUserID: actor, Epoch: body.Epoch, DeviceID: body.DeviceID, AuditRoute: ReclaimPath,
	})
	if err != nil {
		writeError(request.Context(), writer, "reclaim", err)
		return
	}
	if handoff.ReclaimPending {
		desktopwire.WriteJSON(writer, http.StatusAccepted, map[string]any{
			"state": string(handoff.State), "reclaim_pending": true, "epoch": handoff.Epoch,
			"waiting_for_execution_id": handoff.RunningExecutionID,
		})
		return
	}
	desktopwire.WriteJSON(writer, http.StatusOK, map[string]any{
		"state": string(handoff.State), "epoch": handoff.Epoch, "cloud_sha": optionalString(handoff.CloudSHA),
	})
}

func (h *handler) reclaimRequest(writer http.ResponseWriter, request *http.Request) {
	target, ok := thread(writer, request)
	if !ok {
		return
	}
	_, actor, ok := caller(writer, request, false)
	if !ok {
		return
	}
	if err := h.handoff.RequestReclaim(request.Context(), target, actor); err != nil {
		writeError(request.Context(), writer, "reclaim_request", err)
		return
	}
	desktopwire.WriteJSON(writer, http.StatusAccepted, map[string]any{})
}

type releaseBody struct {
	Epoch      int64   `json:"epoch"`
	Outcome    string  `json:"outcome"`
	AppliedSHA *string `json:"applied_sha"`
}

func (h *handler) release(writer http.ResponseWriter, request *http.Request) {
	target, ok := thread(writer, request)
	if !ok {
		return
	}
	_, actor, ok := caller(writer, request, true)
	if !ok {
		return
	}
	var body releaseBody
	if !decodeBody(writer, request, &body) {
		return
	}
	handoff, err := h.handoff.Release(request.Context(), handoffapp.ReleaseRequest{
		Thread: target, ActorUserID: actor, Epoch: body.Epoch, Outcome: body.Outcome,
		AppliedSHA: body.AppliedSHA, AuditRoute: ReleasePath,
	})
	if err != nil {
		writeError(request.Context(), writer, "release", err)
		return
	}
	desktopwire.WriteJSON(writer, http.StatusOK, map[string]any{
		"state": string(handoff.State), "epoch": handoff.Epoch,
		"cloud_copy_delete_after": optionalTimestamp(handoff.DeleteAfter),
	})
}

func desktopCaller(actor int64) workspacestore.Caller {
	return workspacestore.Caller{Writer: workspacestore.WriterDesktop, UserID: actor}
}

type uploadBody struct {
	Epoch  int64  `json:"epoch"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

func (h *handler) upload(writer http.ResponseWriter, request *http.Request) {
	target, ok := thread(writer, request)
	if !ok {
		return
	}
	_, actor, ok := caller(writer, request, true)
	if !ok {
		return
	}
	var body uploadBody
	if !decodeBody(writer, request, &body) {
		return
	}
	grant, err := h.store.RequestUpload(request.Context(), workspacestore.UploadRequest{
		Thread: target, Caller: desktopCaller(actor), Epoch: body.Epoch, SHA256: body.SHA256, Bytes: body.Bytes,
	})
	if err != nil {
		writeError(request.Context(), writer, "upload", err)
		return
	}
	answer := map[string]any{"exists": grant.Exists, "put": nil, "multipart": nil, "expires_at": timestamp(grant.ExpiresAt)}
	if grant.PutURL != "" {
		answer["put"] = map[string]any{"url": grant.PutURL, "headers": grant.Headers}
	}
	if grant.Multipart != nil {
		answer["multipart"] = map[string]any{
			"upload_id": grant.Multipart.UploadID, "part_size": grant.Multipart.PartSize,
			"part_urls": grant.Multipart.PartURLs, "complete_url": grant.Multipart.CompleteURL,
		}
	}
	desktopwire.WriteJSON(writer, http.StatusOK, answer)
}

type uploadCompleteBody struct {
	UploadID string `json:"upload_id"`
	Parts    []struct {
		PartNumber int32  `json:"part_number"`
		ETag       string `json:"etag"`
	} `json:"parts"`
}

func (h *handler) uploadComplete(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := desktopwire.PositiveID(chi.URLParam(request, "projectID"))
	if !ok {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_handoff", "Invalid handoff request")
		return
	}
	_, actor, ok := caller(writer, request, true)
	if !ok {
		return
	}
	var body uploadCompleteBody
	if !decodeBody(writer, request, &body) {
		return
	}
	parts := make([]workspacestore.CompletedPart, 0, len(body.Parts))
	for _, part := range body.Parts {
		parts = append(parts, workspacestore.CompletedPart{Number: part.PartNumber, ETag: part.ETag})
	}
	if err := h.store.CompleteUpload(request.Context(), projectID, desktopCaller(actor),
		chi.URLParam(request, "grantID"), body.UploadID, parts); err != nil {
		writeError(request.Context(), writer, "upload_complete", err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusNoContent)
}

type refBody struct {
	Name string  `json:"name"`
	Old  *string `json:"old"`
	New  string  `json:"new"`
}

type publishBody struct {
	Epoch         int64     `json:"epoch"`
	Kind          string    `json:"kind"`
	SegmentSHA256 string    `json:"segment_sha256"`
	Bytes         int64     `json:"bytes"`
	Prerequisites []string  `json:"prerequisites"`
	Refs          []refBody `json:"refs"`
	ObjectFormat  string    `json:"object_format"`
}

func (h *handler) publish(writer http.ResponseWriter, request *http.Request) {
	target, ok := thread(writer, request)
	if !ok {
		return
	}
	_, actor, ok := caller(writer, request, true)
	if !ok {
		return
	}
	var body publishBody
	if !decodeBody(writer, request, &body) {
		return
	}
	refs := make([]workspacestore.RefUpdate, 0, len(body.Refs))
	for _, ref := range body.Refs {
		refs = append(refs, workspacestore.RefUpdate{Name: ref.Name, Old: ref.Old, New: ref.New})
	}
	prerequisites := body.Prerequisites
	if prerequisites == nil {
		prerequisites = []string{}
	}
	result, err := h.store.Publish(request.Context(), workspacestore.PublishRequest{
		Thread: target, Caller: desktopCaller(actor), Epoch: body.Epoch, Kind: body.Kind,
		SHA256: body.SegmentSHA256, Bytes: body.Bytes, Prerequisites: prerequisites, Refs: refs,
		ObjectFormat: body.ObjectFormat,
	})
	if err != nil {
		writeError(request.Context(), writer, "publish", err)
		return
	}
	superseded := result.Superseded
	if superseded == nil {
		superseded = []int64{}
	}
	desktopwire.WriteJSON(writer, http.StatusOK, map[string]any{"seq": result.Seq, "refs": result.Refs, "superseded": superseded})
}

func (h *handler) fetch(writer http.ResponseWriter, request *http.Request) {
	target, ok := thread(writer, request)
	if !ok {
		return
	}
	_, actor, ok := caller(writer, request, true)
	if !ok {
		return
	}
	since := int64(0)
	if raw := request.URL.Query().Get("since_seq"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_handoff", "since_seq must be a non-negative integer")
			return
		}
		since = value
	}
	plan, err := h.store.Plan(request.Context(), target, desktopCaller(actor), since)
	if err != nil {
		writeError(request.Context(), writer, "fetch", err)
		return
	}
	segments := make([]map[string]any, 0, len(plan.Segments))
	for _, segment := range plan.Segments {
		segments = append(segments, map[string]any{
			"seq": segment.Seq, "kind": segment.Kind, "sha256": segment.SHA256, "bytes": segment.Bytes,
			"prerequisites": segment.Prerequisites, "refs": segment.Refs, "url": segment.URL,
			"expires_at": timestamp(segment.ExpiresAt),
		})
	}
	desktopwire.WriteJSON(writer, http.StatusOK, map[string]any{"head_seq": plan.HeadSeq, "refs": plan.Refs, "segments": segments})
}

func (h *handler) putObject(writer http.ResponseWriter, request *http.Request) {
	target, ok := thread(writer, request)
	if !ok {
		return
	}
	_, actor, ok := caller(writer, request, true)
	if !ok {
		return
	}
	if err := h.store.PutObject(request.Context(), target, desktopCaller(actor), chi.URLParam(request, "sha256"), request.Body); err != nil {
		writeError(request.Context(), writer, "put_object", err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusNoContent)
}

func (h *handler) getObject(writer http.ResponseWriter, request *http.Request) {
	target, ok := thread(writer, request)
	if !ok {
		return
	}
	_, actor, ok := caller(writer, request, true)
	if !ok {
		return
	}
	body, size, err := h.store.OpenObject(request.Context(), target, desktopCaller(actor), chi.URLParam(request, "sha256"))
	if err != nil {
		writeError(request.Context(), writer, "get_object", err)
		return
	}
	defer func() { _ = body.Close() }()
	writer.Header().Set("Content-Type", workspacestore.MediaType)
	writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	writer.Header().Set("Cache-Control", "private, no-store")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.Copy(writer, body)
}

// policy answers the caller's effective handoff policy before a send
// (contract 6.1). The begin answer's limits stay authoritative.
func (h *handler) policy(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := desktopwire.PositiveID(chi.URLParam(request, "projectID"))
	if !ok {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_handoff", "Invalid handoff request")
		return
	}
	if _, _, ok := caller(writer, request, false); !ok {
		return
	}
	policy, err := h.handoff.EffectivePolicy(request.Context(), projectID)
	if err != nil {
		writeError(request.Context(), writer, "policy", err)
		return
	}
	used, err := h.store.ProjectBytes(request.Context(), projectID)
	if err != nil {
		writeError(request.Context(), writer, "policy", err)
		return
	}
	left := policy.Limits.MaxProjectWorkspaceBytes - used
	if left < 0 {
		left = 0
	}
	modes := []string{handoffapp.HistoryNone}
	if policy.HistoryFullAllowed {
		modes = append(modes, handoffapp.HistoryFull)
	}
	var envAllow any
	if policy.EnvAllow != nil {
		envAllow = *policy.EnvAllow
	}
	nonNil := func(values []string) []string {
		if values == nil {
			return []string{}
		}
		return values
	}
	desktopwire.WriteJSON(writer, http.StatusOK, map[string]any{
		"enabled": policy.Enabled, "shell": policy.Shell, "network": policy.Network,
		"history_modes":       modes,
		"exclusions":          nonNil(policy.Exclusions),
		"exclusions_platform": nonNil(policy.ExclusionsPlatform),
		"exclusions_project":  nonNil(policy.ExclusionsProject),
		"path_deny":           nonNil(policy.PathDeny),
		"allow_user_include":  policy.AllowUserInclude,
		"env_harvest":         policy.EnvHarvest,
		"env_deny":            nonNil(policy.EnvDeny),
		"env_allow":           envAllow,
		"limits": map[string]any{
			"max_segment_bytes":            policy.Limits.MaxSegmentBytes,
			"max_file_bytes":               policy.Limits.MaxFileBytes,
			"max_files":                    policy.Limits.MaxFiles,
			"max_unpacked_bytes":           policy.Limits.MaxUnpackedBytes,
			"project_workspace_bytes_left": left,
		},
		"env_limits": map[string]any{
			"max_vars": platformconfig.MaxCloudExecutorEnvVars, "max_bytes": platformconfig.MaxCloudExecutorEnvBytes,
		},
	})
}

// writeError maps the application errors to the contract's codes.
func writeError(ctx context.Context, writer http.ResponseWriter, operation string, err error) {
	var conflict *workspacestore.RefConflictError
	var deniedNames *handoffapp.EnvNameDeniedError
	var deniedPaths *handoffapp.PathDeniedError
	switch {
	case errors.As(err, &deniedNames):
		desktopwire.WriteErrorDetails(writer, http.StatusUnprocessableEntity, "env_name_denied",
			"The policy refuses these environment variable names.", map[string]any{"names": deniedNames.Names})
	case errors.As(err, &deniedPaths):
		desktopwire.WriteErrorDetails(writer, http.StatusUnprocessableEntity, "path_denied",
			"These includes match the path deny list, which nothing can include.", map[string]any{"paths": deniedPaths.Paths})
	case errors.Is(err, handoffapp.ErrIncludeNotAllowed):
		desktopwire.WriteError(writer, http.StatusUnprocessableEntity, "include_not_allowed",
			"Explicit includes are turned off for this project or deployment.")
	case errors.Is(err, handoffapp.ErrEnvHarvestDisabled):
		desktopwire.WriteError(writer, http.StatusUnprocessableEntity, "env_harvest_disabled",
			"Sending environment variables is turned off for this project or deployment.")
	case errors.Is(err, handoffapp.ErrEnvTooLarge):
		desktopwire.WriteError(writer, http.StatusRequestEntityTooLarge, "env_too_large",
			"At most 200 environment variables and 64 KiB in all.")
	case errors.As(err, &conflict):
		current := map[string]any{}
		for name, value := range conflict.Current {
			current[name] = optionalString(value)
		}
		desktopwire.WriteErrorDetails(writer, http.StatusConflict, "ref_conflict",
			"A ref moved since the caller read it.", map[string]any{"current": current})
	case errors.Is(err, handoffapp.ErrInvalid), errors.Is(err, workspacestore.ErrInvalid),
		errors.Is(err, handoffapp.ErrOverLimits), errors.Is(err, handoffapp.ErrNotLocalWork),
		errors.Is(err, handoffapp.ErrRefsMissing):
		desktopwire.WriteError(writer, http.StatusUnprocessableEntity, "invalid_handoff", invalidMessage(err))
	case errors.Is(err, handoffapp.ErrNotFound), errors.Is(err, workspacestore.ErrNotFound):
		desktopwire.WriteError(writer, http.StatusNotFound, "handoff_not_found",
			"No handoff of this thread for this caller.")
	case errors.Is(err, handoffapp.ErrDisabled):
		desktopwire.WriteError(writer, http.StatusForbidden, "cloud_handoff_disabled",
			"Send to cloud is turned off for this project or deployment.")
	case errors.Is(err, handoffapp.ErrExecutorIsCloud):
		desktopwire.WriteError(writer, http.StatusConflict, "thread_executor_is_cloud", "The cloud executor owns this thread.")
	case errors.Is(err, handoffapp.ErrInProgress):
		desktopwire.WriteError(writer, http.StatusConflict, "handoff_in_progress",
			"A handoff, a take back or an erasure of this thread is under way. Retry shortly.")
	case errors.Is(err, handoffapp.ErrLocalTurnInProgress):
		desktopwire.WriteError(writer, http.StatusConflict, "local_turn_in_progress",
			"A local turn of this thread is not committed yet.")
	case errors.Is(err, handoffapp.ErrNotInCloud):
		desktopwire.WriteError(writer, http.StatusConflict, "handoff_not_in_cloud", "This thread is not with the cloud executor.")
	case errors.Is(err, handoffapp.ErrErased):
		desktopwire.WriteError(writer, http.StatusConflict, "handoff_erased",
			"The cloud copy of this thread was erased, so nothing was handed back.")
	case errors.Is(err, handoffapp.ErrStaleEpoch), errors.Is(err, workspacestore.ErrStaleEpoch):
		desktopwire.WriteError(writer, http.StatusConflict, "stale_epoch",
			"The thread's state changed. Read the state and retry with its epoch.")
	case errors.Is(err, handoffapp.ErrHistoryModeNotAllowed):
		desktopwire.WriteError(writer, http.StatusUnprocessableEntity, "history_mode_not_allowed",
			"Full history is not allowed for this project.")
	case errors.Is(err, handoffapp.ErrQuotaExceeded), errors.Is(err, workspacestore.ErrQuotaExceeded):
		desktopwire.WriteError(writer, http.StatusRequestEntityTooLarge, "workspace_quota_exceeded",
			"The project's cloud copies would pass their storage ceiling.")
	case errors.Is(err, workspacestore.ErrSegmentTooLarge):
		desktopwire.WriteError(writer, http.StatusRequestEntityTooLarge, "segment_too_large", "The segment is over the size cap.")
	case errors.Is(err, workspacestore.ErrRefNotAllowed):
		desktopwire.WriteError(writer, http.StatusForbidden, "ref_not_allowed", "This writer may not move that ref now.")
	case errors.Is(err, workspacestore.ErrSegmentMissing):
		desktopwire.WriteError(writer, http.StatusUnprocessableEntity, "segment_missing", "The segment is not uploaded.")
	case errors.Is(err, workspacestore.ErrChecksumMismatch):
		desktopwire.WriteError(writer, http.StatusUnprocessableEntity, "checksum_mismatch",
			"The stored segment's size or SHA-256 differs from the declared one.")
	case errors.Is(err, workspacestore.ErrUnsupportedObjectFormat):
		desktopwire.WriteError(writer, http.StatusUnprocessableEntity, "unsupported_object_format",
			"Only sha1 git repositories can be sent in this version.")
	case errors.Is(err, handoffapp.ErrUnavailable), errors.Is(err, workspacestore.ErrUnavailable):
		writer.Header().Set("Retry-After", "1")
		desktopwire.WriteError(writer, http.StatusServiceUnavailable, "handoff_unavailable", "Retry with the same body.")
	default:
		slog.ErrorContext(ctx, "workspace handoff failed", "operation", operation, "err", err)
		desktopwire.WriteError(writer, http.StatusInternalServerError, "handoff_failed", "The handoff request could not be served.")
	}
}

func invalidMessage(err error) string {
	switch {
	case errors.Is(err, handoffapp.ErrOverLimits):
		return "The send is over a cap (files, upload size or unpacked size)."
	case errors.Is(err, handoffapp.ErrNotLocalWork):
		return "Only a desktop Local work thread can be sent to the cloud."
	case errors.Is(err, handoffapp.ErrRefsMissing):
		return "The store does not hold the snapshot this call names. Publish it first."
	}
	return "Invalid handoff request"
}
