package eliteacore

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// Feedback per listing version (ADR-0051 decision 5, issue 1631): the read, the
// review of a verified hire, the workforce report a person approves, and the
// publisher's and administrator's actions. The store is
// internal/marketplace/feedback.go.
//
// No response carries a project id or a project name. A review names its author
// and a workforce report names only "a hiring project".

type feedbackView struct {
	marketplace.FeedbackPage
	Signals marketplace.SignalsSet `json:"signals"`
	// ReviewVersions are the versions the caller's project hired, the ones its
	// people may review. Empty when the project never hired the listing.
	ReviewVersions []marketplace.ReviewVersion `json:"review_versions"`
	CanReview      bool                        `json:"can_review"`
	CanReply       bool                        `json:"can_reply"`
	CanFlag        bool                        `json:"can_flag"`
	CanModerate    bool                        `json:"can_moderate"`
}

func writeFeedbackError(w http.ResponseWriter, err error) bool {
	var paramErr *marketplace.ParamError
	switch {
	case err == nil:
		return false
	case errors.As(err, &paramErr):
		writeMarketplaceParamError(w, err)
	case errors.Is(err, marketplace.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	case errors.Is(err, marketplace.ErrNotVerifiedHire):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "only a project that hired this listing may leave feedback", "code": "not_verified_hire"})
	case errors.Is(err, marketplace.ErrOwnListing):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "feedback on your own listing is not allowed", "code": "own_listing"})
	case errors.Is(err, marketplace.ErrNotWorkforceManager):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "the agent is not a workforce manager of this project", "code": "not_workforce_manager"})
	case errors.Is(err, marketplace.ErrAlreadyDone):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "already done", "code": "already_done"})
	case errors.Is(err, marketplace.ErrInvalidRequest):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
	default:
		return false
	}
	return true
}

func parseFeedbackIDs(w http.ResponseWriter, r *http.Request) (projectID, listingID int64, ok bool) {
	projectID = parseProjectID(chi.URLParam(r, "projectID"))
	listingID, err := strconv.ParseInt(chi.URLParam(r, "listingID"), 10, 64)
	if projectID <= 0 || err != nil || listingID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project or listing id"})
		return 0, 0, false
	}
	return projectID, listingID, true
}

// readableListing answers the listing the caller may read, with their role on
// it. It writes the error response and answers false for a listing the caller
// may not read.
func (h *Handler) readableListing(w http.ResponseWriter, r *http.Request, projectID, listingID int64) (marketplace.Detail, bool) {
	ctx := r.Context()
	h.ensureListings(ctx)
	detail, err := marketplace.Get(ctx, h.pool, marketplace.GetParams{
		ListingID: listingID, ProjectID: projectID, UserID: callerUserID(ctx), PublicSchema: publicTenantSchema(),
		Moderator: h.holdsModerationPermission(ctx),
	})
	if errors.Is(err, marketplace.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "listing not found"})
		return marketplace.Detail{}, false
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace: read the listing failed", "listing_id", listingID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read the listing"})
		return marketplace.Detail{}, false
	}
	return detail, true
}

// MarketplaceFeedback answers GET
// /elitea_core/marketplace_feedback/prompt_lib/{projectID}/{listingID}: a page
// of items, the rating distribution per version, the signals, and what the
// caller may do.
func (h *Handler) MarketplaceFeedback(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the marketplace needs a database"})
		return
	}
	ctx := r.Context()
	projectID, listingID, ok := parseFeedbackIDs(w, r)
	if !ok {
		return
	}
	detail, ok := h.readableListing(w, r, projectID, listingID)
	if !ok {
		return
	}
	values := r.URL.Query()
	query := marketplace.FeedbackQuery{
		ListingID: listingID, Kind: values.Get("kind"), UserID: callerUserID(ctx), ProjectID: projectID,
		Moderator: detail.CanModerate, Publisher: detail.CanManage,
	}
	if raw := values.Get("version_id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			writeMarketplaceParamError(w, &marketplace.ParamError{Param: "version_id", Message: "must be a positive integer"})
			return
		}
		query.VersionID = parsed
	}
	for _, field := range []struct {
		name string
		into *int
	}{{"limit", &query.Limit}, {"offset", &query.Offset}} {
		if raw := values.Get(field.name); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 {
				writeMarketplaceParamError(w, &marketplace.ParamError{Param: field.name, Message: "must be a non-negative integer"})
				return
			}
			*field.into = parsed
		}
	}
	page, err := marketplace.ListFeedback(ctx, h.pool, query)
	if writeFeedbackError(w, err) {
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace: read the feedback failed", "listing_id", listingID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read the feedback"})
		return
	}
	signals, err := marketplace.ReadSignals(ctx, h.pool, listingID)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace: read the signals failed", "listing_id", listingID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read the signals"})
		return
	}
	view := feedbackView{
		FeedbackPage: page, Signals: signals, ReviewVersions: []marketplace.ReviewVersion{},
		CanReply: detail.CanManage, CanFlag: detail.CanManage || detail.CanModerate, CanModerate: detail.CanModerate,
	}
	if !detail.CanManage {
		view.ReviewVersions, err = marketplace.ReviewableVersions(ctx, h.pool, projectID, listingID)
		if err != nil {
			slog.ErrorContext(ctx, "marketplace: read the reviewable versions failed", "listing_id", listingID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read the feedback"})
			return
		}
		view.CanReview = len(view.ReviewVersions) > 0
	}
	writeJSON(w, http.StatusOK, view)
}

type reviewBody struct {
	VersionID string `json:"version_id"`
	Rating    int    `json:"rating"`
	Body      string `json:"body"`
}

// MarketplaceFeedbackReview answers POST
// /elitea_core/marketplace_feedback/prompt_lib/{projectID}/{listingID}: the
// caller's review of a listing their project hired. A second call edits it.
func (h *Handler) MarketplaceFeedbackReview(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the marketplace needs a database"})
		return
	}
	ctx := r.Context()
	projectID, listingID, ok := parseFeedbackIDs(w, r)
	if !ok {
		return
	}
	var body reviewBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	caller := callerUserID(ctx)
	if caller <= 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "a signed-in person is required"})
		return
	}
	var versionID int64
	if body.VersionID != "" {
		parsed, err := strconv.ParseInt(body.VersionID, 10, 64)
		if err != nil || parsed <= 0 {
			writeMarketplaceParamError(w, &marketplace.ParamError{Param: "version_id", Message: "must be a positive integer"})
			return
		}
		versionID = parsed
	}
	id, created, err := marketplace.SaveReview(ctx, h.pool, marketplace.ReviewInput{
		ListingID: listingID, VersionID: versionID, ProjectID: projectID, UserID: caller,
		Rating: body.Rating, Body: body.Body,
	})
	if writeFeedbackError(w, err) {
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace: save the review failed", "listing_id", listingID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to save the review"})
		return
	}
	audit.Annotate(ctx, audit.Annotation{Action: "marketplace.review.save", EntityType: "marketplace_feedback", EntityID: audit.ID(id)})
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"id": strconv.FormatInt(id, 10), "created": created})
}

type reportBody struct {
	HiredAgentID int64  `json:"hired_agent_id"`
	AgentID      int64  `json:"agent_id"`
	Outcome      string `json:"outcome"`
	Stage        string `json:"stage"`
	ReasonCode   string `json:"reason_code"`
	Text         string `json:"text"`
}

// MarketplaceFeedbackReport answers POST
// /elitea_core/marketplace_feedback_report/prompt_lib/{projectID}/{listingID}:
// a workforce manager's report on a hired agent, published by the person who
// approves it (ADR-0051 decision 6). The scorecard summary is filled by the
// platform, never by the request, and the auto_feedback policy path is not
// reachable from here: it is a call of the store by the platform.
func (h *Handler) MarketplaceFeedbackReport(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the marketplace needs a database"})
		return
	}
	ctx := r.Context()
	projectID, listingID, ok := parseFeedbackIDs(w, r)
	if !ok {
		return
	}
	var body reportBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	caller := callerUserID(ctx)
	if caller <= 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "a signed-in person is required"})
		return
	}
	id, err := marketplace.SaveWorkforceReport(ctx, h.pool, marketplace.ReportInput{
		ListingID: listingID, ProjectID: projectID, HiredEntityID: body.HiredAgentID, AgentID: body.AgentID,
		ApprovedBy: caller, Outcome: body.Outcome, Stage: body.Stage, ReasonCode: body.ReasonCode, Body: body.Text,
	})
	if writeFeedbackError(w, err) {
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace: save the workforce report failed", "listing_id", listingID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to save the report"})
		return
	}
	audit.Annotate(ctx, audit.Annotation{Action: "marketplace.workforce_report.save", EntityType: "marketplace_feedback", EntityID: audit.ID(id)})
	writeJSON(w, http.StatusCreated, map[string]any{"id": strconv.FormatInt(id, 10)})
}

type feedbackActionBody struct {
	Action string `json:"action"`
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

// MarketplaceFeedbackAction answers POST
// /elitea_core/marketplace_feedback_action/prompt_lib/{projectID}/{listingID}/{feedbackID}.
// The publisher replies to an item once and flags it. A platform administrator
// flags, unflags, hides and unhides, and every change writes an audit event.
func (h *Handler) MarketplaceFeedbackAction(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the marketplace needs a database"})
		return
	}
	ctx := r.Context()
	projectID, listingID, ok := parseFeedbackIDs(w, r)
	if !ok {
		return
	}
	feedbackID, err := strconv.ParseInt(chi.URLParam(r, "feedbackID"), 10, 64)
	if err != nil || feedbackID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid feedback id"})
		return
	}
	var body feedbackActionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	caller := callerUserID(ctx)
	if caller <= 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "a signed-in person is required"})
		return
	}
	detail, ok := h.readableListing(w, r, projectID, listingID)
	if !ok {
		return
	}
	publisher, moderator := detail.CanManage, detail.CanModerate
	var actionErr error
	switch body.Action {
	case "reply":
		if !publisher {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "only the publisher may reply"})
			return
		}
		actionErr = marketplace.Reply(ctx, h.pool, listingID, feedbackID, body.Text, caller)
	case "flag", "unflag":
		if !publisher && !moderator {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "only the publisher or an administrator may flag"})
			return
		}
		actionErr = marketplace.Flag(ctx, h.pool, listingID, feedbackID, body.Action == "flag", body.Reason, caller)
	case "hide", "unhide":
		if !moderator {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "only a platform administrator may hide an item"})
			return
		}
		actionErr = marketplace.SetHidden(ctx, h.pool, listingID, feedbackID, body.Action == "hide", body.Reason, caller)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "action must be reply, flag, unflag, hide or unhide"})
		return
	}
	if writeFeedbackError(w, actionErr) {
		return
	}
	if actionErr != nil {
		slog.ErrorContext(ctx, "marketplace: feedback action failed", "listing_id", listingID, "feedback_id", feedbackID, "action", body.Action, "error", actionErr)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to change the item"})
		return
	}
	audit.Annotate(ctx, audit.Annotation{
		Action: "marketplace.feedback." + body.Action, EntityType: "marketplace_feedback", EntityID: audit.ID(feedbackID),
	})
	slog.InfoContext(ctx, "marketplace: feedback changed", "listing_id", listingID, "feedback_id", feedbackID, "action", body.Action, "by", caller)
	writeJSON(w, http.StatusOK, map[string]any{"id": strconv.FormatInt(feedbackID, 10), "action": body.Action})
}
