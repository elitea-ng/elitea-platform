package eliteacore

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// The Marketplace reads (ADR-0051 decision 2): the facet search, the listing
// page and the origin of a hired agent. The writes are in marketplace_manage.go
// and marketplace_hire.go.

func csvParam(values url.Values, key string) []string {
	out := []string{}
	for _, part := range strings.Split(values.Get(key), ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func writeMarketplaceParamError(w http.ResponseWriter, err error) {
	body := map[string]any{"error": "invalid_query_parameter", "error_description": err.Error()}
	var paramErr *marketplace.ParamError
	if errors.As(err, &paramErr) {
		body["param"] = paramErr.Param
		body["msg"] = paramErr.Message
	}
	writeJSON(w, http.StatusBadRequest, body)
}

// callerUserID is the caller's numeric user id, or 0 for a principal with none.
func callerUserID(ctx context.Context) int64 {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return 0
	}
	parsed, err := strconv.ParseInt(user.ID, 10, 64)
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

// holdsModerationPermission asks whether the caller is a platform
// administrator for the Marketplace: the permission of the admin published
// agents view, resolved in administration mode.
func (h *Handler) holdsModerationPermission(ctx context.Context) bool {
	if h.permissionResolver == nil {
		return false
	}
	principal, ok := auth.UserFromContext(ctx)
	if !ok {
		return false
	}
	resolution, err := h.permissionResolver.ResolvePermissions(ctx, principal, auth.PermissionModeAdministration, "")
	if err != nil {
		return false
	}
	for _, granted := range resolution.Permissions {
		if granted == PublishedAgentsListPermission {
			return true
		}
	}
	return false
}

// ensureListings builds the listings of versions published before the
// Marketplace existed, once per process after the first success.
func (h *Handler) ensureListings(ctx context.Context) {
	h.listingsOnce.Do(func() {
		publicID, _ := strconv.ParseInt(publicProjectIDOrDefault(), 10, 64)
		h.listingsBackfill = marketplace.NewBackfill(h.pool, publicTenantSchema(), publicID)
	})
	if err := h.listingsBackfill.Ensure(ctx); err != nil {
		slog.WarnContext(ctx, "marketplace: backfill of the listings failed; reads go on with what exists", "error", err)
	}
}

// MarketplaceSearch answers GET /elitea_core/marketplace_listings/prompt_lib/{projectID}.
func (h *Handler) MarketplaceSearch(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the marketplace needs a database"})
		return
	}
	ctx := r.Context()
	projectID := parseProjectID(chi.URLParam(r, "projectID"))
	if projectID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}
	values := r.URL.Query()
	params := marketplace.SearchParams{
		ProjectID: projectID, UserID: callerUserID(ctx), PublicSchema: publicTenantSchema(),
		Kind: values.Get("kind"), Query: strings.TrimSpace(values.Get("query")),
		Categories: csvParam(values, "category"), Tags: csvParam(values, "tag"), Requires: csvParam(values, "requires"),
		MaxTier: values.Get("max_tier"), Sort: values.Get("sort"),
		HiredHere: parseBoolParam(values.Get("hired_here")), FeaturedOnly: parseBoolParam(values.Get("featured")),
	}
	for _, field := range []struct {
		name string
		into *int64
	}{{"publisher_id", &params.PublisherID}, {"entity_id", &params.EntityID}} {
		if raw := values.Get(field.name); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || parsed <= 0 {
				writeMarketplaceParamError(w, &marketplace.ParamError{Param: field.name, Message: "must be a positive integer"})
				return
			}
			*field.into = parsed
		}
	}
	if raw := values.Get("min_rating"); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			writeMarketplaceParamError(w, &marketplace.ParamError{Param: "min_rating", Message: "must be a number"})
			return
		}
		params.MinRating = parsed
	}
	for _, field := range []struct {
		name string
		into *int
	}{{"limit", &params.Limit}, {"offset", &params.Offset}} {
		if raw := values.Get(field.name); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				writeMarketplaceParamError(w, &marketplace.ParamError{Param: field.name, Message: "must be a whole number"})
				return
			}
			*field.into = parsed
		}
	}
	if err := params.Validate(); err != nil {
		writeMarketplaceParamError(w, err)
		return
	}
	h.ensureListings(ctx)
	page, err := marketplace.Search(ctx, h.pool, params)
	if err != nil {
		var paramErr *marketplace.ParamError
		if errors.As(err, &paramErr) {
			writeMarketplaceParamError(w, err)
			return
		}
		slog.ErrorContext(ctx, "marketplace: search failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to search the marketplace"})
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// MarketplaceListing answers GET /elitea_core/marketplace_listing/prompt_lib/{projectID}/{listingID}.
func (h *Handler) MarketplaceListing(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the marketplace needs a database"})
		return
	}
	ctx := r.Context()
	projectID := parseProjectID(chi.URLParam(r, "projectID"))
	listingID, err := strconv.ParseInt(chi.URLParam(r, "listingID"), 10, 64)
	if projectID <= 0 || err != nil || listingID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project or listing id"})
		return
	}
	h.ensureListings(ctx)
	detail, err := marketplace.Get(ctx, h.pool, marketplace.GetParams{
		ListingID: listingID, ProjectID: projectID, UserID: callerUserID(ctx), PublicSchema: publicTenantSchema(),
		Moderator: h.holdsModerationPermission(ctx),
	})
	if errors.Is(err, marketplace.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "listing not found"})
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace: read the listing failed", "listing_id", listingID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read the listing"})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// MarketplaceOrigin answers GET /elitea_core/marketplace_origin/prompt_lib/{projectID}/{applicationID}.
func (h *Handler) MarketplaceOrigin(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the marketplace needs a database"})
		return
	}
	ctx := r.Context()
	schema, ok := tenantSchema(w, chi.URLParam(r, "projectID"))
	if !ok {
		return
	}
	applicationID, err := strconv.ParseInt(chi.URLParam(r, "applicationID"), 10, 64)
	if err != nil || applicationID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid application id"})
		return
	}
	origin, err := marketplace.GetOrigin(ctx, h.pool, schema, applicationID, marketplace.OriginLookup{
		ProjectID: parseProjectID(chi.URLParam(r, "projectID")), PublicProjectID: publicProjectIDOrDefault(),
	})
	if errors.Is(err, marketplace.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "the agent was not hired from the marketplace"})
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace: read the origin failed", "application_id", applicationID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read the origin"})
		return
	}
	writeJSON(w, http.StatusOK, origin)
}
