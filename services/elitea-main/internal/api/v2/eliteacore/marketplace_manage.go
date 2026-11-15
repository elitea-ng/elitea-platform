package eliteacore

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// The publisher and administrator actions of the Marketplace (ADR-0051
// decision 2): deprecate, yank and restore a version, and feature, delist or
// list a listing.
//
// The publisher is the person who published the listing last. A platform
// administrator is a caller who holds the admin published agents permission in
// administration mode. A version an administrator yanked stays yanked until an
// administrator restores it: the publisher cannot undo a moderation.

type versionStateBody struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// MarketplaceVersionState answers POST
// /elitea_core/marketplace_listing_version/prompt_lib/{projectID}/{listingID}/{versionID}.
func (h *Handler) MarketplaceVersionState(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the marketplace needs a database"})
		return
	}
	ctx := r.Context()
	listingID, listingErr := strconv.ParseInt(chi.URLParam(r, "listingID"), 10, 64)
	versionID, versionErr := strconv.ParseInt(chi.URLParam(r, "versionID"), 10, 64)
	if listingErr != nil || versionErr != nil || listingID <= 0 || versionID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid listing or version id"})
		return
	}
	var body versionStateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	switch body.State {
	case marketplace.VersionAvailable, marketplace.VersionDeprecated, marketplace.VersionYanked:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state must be available, deprecated or yanked"})
		return
	}
	caller := callerUserID(ctx)
	if caller <= 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "a signed-in person is required"})
		return
	}
	publisher, err := marketplace.PublisherOf(ctx, h.pool, listingID)
	if errors.Is(err, marketplace.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "listing not found"})
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace: read the publisher failed", "listing_id", listingID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to change the version"})
		return
	}
	isPublisher := publisher > 0 && publisher == caller
	moderator := h.holdsModerationPermission(ctx)
	if !isPublisher && !moderator {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "only the publisher or an administrator may change a version"})
		return
	}
	var currentState string
	var changedBy *int64
	if err := h.pool.QueryRow(ctx, `
SELECT state, state_changed_by FROM elitea_marketplace.listing_versions WHERE listing_id = $1 AND version_id = $2`,
		listingID, versionID).Scan(&currentState, &changedBy); errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "version not found"})
		return
	} else if err != nil {
		slog.ErrorContext(ctx, "marketplace: read the version state failed", "listing_id", listingID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to change the version"})
		return
	}
	// An administrator who is not the publisher may only yank. A yank by an
	// administrator is lifted by an administrator.
	// An administrator who is not the publisher may also lift a yank.
	liftsYank := currentState == marketplace.VersionYanked && body.State == marketplace.VersionAvailable
	if !isPublisher && body.State != marketplace.VersionYanked && !liftsYank {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "an administrator may only yank a version"})
		return
	}
	moderatedYank := currentState == marketplace.VersionYanked && changedBy != nil && *changedBy != publisher
	if moderatedYank && !moderator {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "an administrator yanked this version; only an administrator may restore it"})
		return
	}
	found, err := marketplace.SetVersionState(ctx, h.pool, listingID, versionID, body.State, body.Reason, caller)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace: set the version state failed", "listing_id", listingID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to change the version"})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "version not found"})
		return
	}
	slog.InfoContext(ctx, "marketplace: version state changed",
		"listing_id", listingID, "version_id", versionID, "state", body.State, "by", caller, "moderator", moderator && !isPublisher)
	if h.events != nil {
		h.events.Emit(ctx, chi.URLParam(r, "projectID"), "marketplace.version.state_changed", map[string]any{
			"listing_id": listingID, "version_id": versionID, "state": body.State,
		})
	}
	detail, err := marketplace.Get(ctx, h.pool, marketplace.GetParams{
		ListingID: listingID, ProjectID: parseProjectID(chi.URLParam(r, "projectID")), UserID: caller,
		PublicSchema: publicTenantSchema(), Moderator: true,
	})
	if err != nil {
		slog.ErrorContext(ctx, "marketplace: re-read the listing failed", "listing_id", listingID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read the version"})
		return
	}
	for _, version := range detail.Versions {
		if version.ID == strconv.FormatInt(versionID, 10) {
			writeJSON(w, http.StatusOK, version)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "version not found"})
}

type listingUpdateBody struct {
	Featured *bool   `json:"featured"`
	State    *string `json:"state"`
	Reason   string  `json:"reason"`
}

// MarketplaceListingUpdate answers PATCH
// /elitea_core/marketplace_listing/prompt_lib/{projectID}/{listingID}: the
// editorial Featured flag and the administrator's delist.
func (h *Handler) MarketplaceListingUpdate(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the marketplace needs a database"})
		return
	}
	ctx := r.Context()
	listingID, err := strconv.ParseInt(chi.URLParam(r, "listingID"), 10, 64)
	if err != nil || listingID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid listing id"})
		return
	}
	var body listingUpdateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.Featured == nil && body.State == nil) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "send featured or state"})
		return
	}
	if body.State != nil && *body.State != marketplace.ListingListed && *body.State != marketplace.ListingDelisted {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state must be listed or delisted"})
		return
	}
	caller := callerUserID(ctx)
	if caller <= 0 || !h.holdsModerationPermission(ctx) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "only a platform administrator may feature or delist a listing"})
		return
	}
	if body.Featured != nil {
		found, err := marketplace.SetFeatured(ctx, h.pool, listingID, *body.Featured, caller)
		if err != nil {
			slog.ErrorContext(ctx, "marketplace: set the featured flag failed", "listing_id", listingID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to change the listing"})
			return
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "listing not found"})
			return
		}
	}
	if body.State != nil {
		found, err := marketplace.SetListed(ctx, h.pool, listingID, *body.State == marketplace.ListingListed, body.Reason, caller)
		if err != nil {
			slog.ErrorContext(ctx, "marketplace: set the listing state failed", "listing_id", listingID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to change the listing"})
			return
		}
		if !found {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "the listing has no published version to list"})
			return
		}
	}
	slog.InfoContext(ctx, "marketplace: listing changed by an administrator", "listing_id", listingID, "by", caller)
	detail, err := marketplace.Get(ctx, h.pool, marketplace.GetParams{
		ListingID: listingID, ProjectID: parseProjectID(chi.URLParam(r, "projectID")), UserID: caller,
		PublicSchema: publicTenantSchema(), Moderator: true,
	})
	if errors.Is(err, marketplace.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "listing not found"})
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace: re-read the listing failed", "listing_id", listingID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read the listing"})
		return
	}
	writeJSON(w, http.StatusOK, detail.Listing)
}
