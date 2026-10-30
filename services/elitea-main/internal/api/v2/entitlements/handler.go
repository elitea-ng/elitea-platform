// Package entitlements serves the admin entitlement export (ADR-0044
// decision 5, issue 1501):
//
//	GET /api/v2/admin/entitlements?format=csv|json
//
// behind entitlements.export in the administration mode (the router applies
// it). CSV is streamed from one database snapshot, every matching row; JSON
// is paged (limit, offset). Every export writes an audit_events row naming
// the caller, the format and the row count BEFORE the first byte leaves; an
// export the trail cannot record is refused with 503.
//
// The row shape and the capability vocabulary are internal/domain/entitlements.
// The same grants are served to identity governance tools as SCIM resources
// under /api/v2/scim/v2/entitlements (internal/api/scim/entitlements.go).
package entitlements

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/entitlements"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// AdminPath is the route, relative to the /api/v2/admin group.
const AdminPath = "/entitlements"

// RowsHeader carries the number of data rows a CSV export holds, so an
// importer can tell a complete file from one cut short.
const RowsHeader = "X-Entitlement-Rows"

const (
	defaultPage = 100
	maxPage     = 500
)

// Store is *repos.Entitlements.
type Store interface {
	Stream(ctx context.Context, filter repos.EntitlementFilter,
		before func(rows int) error, emit func(domain.Grant) error) error
	Page(ctx context.Context, filter repos.EntitlementFilter, limit, offset int) ([]domain.Grant, int, error)
	RecordExport(ctx context.Context, record repos.EntitlementExportAudit) error
}

// Handler serves the export.
type Handler struct {
	store Store
}

// NewHandler returns the handler; a nil store answers 503.
func NewHandler(store Store) *Handler {
	return &Handler{store: store}
}

// Grant is one JSON row; the fields are the CSV columns.
type Grant struct {
	UserID      int64    `json:"user_id"`
	UserName    string   `json:"user_name"`
	DisplayName string   `json:"display_name"`
	UserActive  bool     `json:"user_active"`
	ScopeType   string   `json:"scope_type"`
	ScopeID     string   `json:"scope_id"`
	ScopeName   string   `json:"scope_name"`
	ProjectID   *int64   `json:"project_id"`
	ScopeActive bool     `json:"scope_active"`
	Role        string   `json:"role"`
	Capability  *string  `json:"capability"`
	Permissions []string `json:"permissions"`
	GrantedVia  string   `json:"granted_via"`
}

func view(g domain.Grant) Grant {
	out := Grant{
		UserID: g.UserID, UserName: g.UserName, DisplayName: g.DisplayName, UserActive: g.UserActive,
		ScopeType: g.ScopeType, ScopeID: g.ScopeID, ScopeName: g.ScopeName, ScopeActive: g.ScopeActive,
		Role: g.Role, Permissions: g.Permissions, GrantedVia: g.GrantedVia,
	}
	if out.Permissions == nil {
		out.Permissions = []string{}
	}
	if g.ProjectID > 0 {
		project := g.ProjectID
		out.ProjectID = &project
	}
	if g.Capability != "" {
		capability := g.Capability
		out.Capability = &capability
	}
	return out
}

var errNotRecorded = errors.New("entitlements: the export could not be recorded")

// Export answers GET /admin/entitlements.
func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.store == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the entitlement export is not available on this deployment")
		return
	}
	principal, _ := auth.UserFromContext(r.Context())
	userID, ok := principal.OwningUserID()
	if !ok || userID <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "the entitlement export needs a signed-in user")
		return
	}
	query := r.URL.Query()
	format := query.Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" {
		apierr.WriteStatus(w, http.StatusBadRequest, "format is json or csv")
		return
	}
	var filter repos.EntitlementFilter
	for _, field := range []struct {
		name   string
		target *int64
	}{{"user_id", &filter.UserID}, {"project_id", &filter.ProjectID}} {
		if raw := query.Get(field.name); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value <= 0 {
				apierr.WriteStatus(w, http.StatusBadRequest, field.name+" is a positive id")
				return
			}
			*field.target = value
		}
	}
	route := chi.RouteContext(r.Context()).RoutePattern()
	record := func(rows int) error {
		err := h.store.RecordExport(r.Context(), repos.EntitlementExportAudit{
			UserID: userID, Actor: principal.Email, Format: format, Rows: rows, Route: route,
		})
		if err != nil {
			slog.ErrorContext(r.Context(), "entitlements: export not audited, refusing it", "format", format, "error", err)
			return errNotRecorded
		}
		return nil
	}
	if format == "csv" {
		h.exportCSV(w, r, filter, record)
		return
	}

	limit, offset := defaultPage, 0
	for _, field := range []struct {
		name   string
		target *int
		max    int
	}{{"limit", &limit, maxPage}, {"offset", &offset, 1 << 30}} {
		if raw := query.Get(field.name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 || value > field.max {
				apierr.WriteStatus(w, http.StatusBadRequest,
					field.name+" is an integer from 0 to "+strconv.Itoa(field.max))
				return
			}
			*field.target = value
		}
	}
	if limit == 0 {
		limit = defaultPage
	}
	grants, total, err := h.store.Page(r.Context(), filter, limit, offset)
	if err != nil {
		slog.ErrorContext(r.Context(), "entitlements: page", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "could not read the entitlements")
		return
	}
	if err := record(len(grants)); err != nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the export could not be recorded in the audit trail")
		return
	}
	rows := make([]Grant, 0, len(grants))
	for _, grant := range grants {
		rows = append(rows, view(grant))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"total": total, "rows": rows})
}

func (h *Handler) exportCSV(w http.ResponseWriter, r *http.Request, filter repos.EntitlementFilter, record func(int) error) {
	var writer *domain.CSVWriter
	err := h.store.Stream(r.Context(), filter, func(rows int) error {
		if err := record(rows); err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="entitlements.csv"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set(RowsHeader, strconv.Itoa(rows))
		w.WriteHeader(http.StatusOK)
		var err error
		writer, err = domain.NewCSVWriter(w)
		return err
	}, func(grant domain.Grant) error {
		return writer.Write(grant)
	})
	switch {
	case writer == nil && errors.Is(err, errNotRecorded):
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the export could not be recorded in the audit trail")
		return
	case writer == nil && err != nil:
		slog.ErrorContext(r.Context(), "entitlements: csv export", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "could not read the entitlements")
		return
	case err != nil:
		// The status is sent. The file is short of the row count the
		// header announced, which is how an importer detects it.
		slog.ErrorContext(r.Context(), "entitlements: csv export cut short", "rows", writer.Rows(), "error", err)
	}
	if closeErr := writer.Close(); closeErr != nil {
		slog.ErrorContext(r.Context(), "entitlements: csv flush", "error", closeErr)
	}
}
