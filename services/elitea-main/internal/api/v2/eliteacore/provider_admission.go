package eliteacore

// The provider ADMISSION plane behind the three service-descriptor routes
// (ADR-0012 phase P3, migration 0107).
//
// WHAT CHANGED, AND WHAT DID NOT. These routes answered 501 because there was
// no store — see service_descriptors.go's header for the full account. There is
// one now, so they answer honestly instead of refusing.
//
// THIS PARAGRAPH USED TO SAY THIS DEPLOYMENT COULD NOT ACTIVATE A PROVIDER, and
// it was true until migration 0109. The limit was a database constraint
// (provider_admitted_revision_active_needs_overlay) rather than a branch in this
// file, so it could not be lifted by editing Go — and it has NOT been lifted.
// 0109 adds the policy overlay table the constraint demands, plus the FOREIGN
// KEY that makes overlay_revision name a real reviewed policy instead of any
// non-empty string. Activation lives in provider_activation.go, behind its own
// permission. Nothing in THIS file activates anything: registration still
// records an inactive revision, and DELETE still revokes.
//
// THREE CONTRACT DECISIONS, each chosen against the shape this replaces:
//
//   * `healthy` is a THREE-STATE value: true, false, or null. Null means no
//     probe has reported inside the freshness window. The legacy page could
//     only say true or false, because its value was which in-process dict an
//     entry landed in — so a provider nobody had probed appeared unhealthy, and
//     an operator could not tell that from one that had actually failed.
//
//   * Registration answers 202, not 200. The descriptor is recorded and is NOT
//     in force: 200 would say the provider is admitted, which is the thing this
//     deployment cannot do. The body carries the revision id, the manifest
//     digest, the status and the reason, so a caller learns what happened
//     rather than being told `{"ok": true}` — which is what the handler this
//     replaces returned, without storing anything.
//
//   * DELETE REVOKES; it never deletes a row. An admission that was once in
//     force is a fact about what this deployment ran, and an audit that can be
//     erased by the surface it audits is not one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhub"
	"github.com/go-chi/chi/v5"
)

// providerHealthFreshness bounds how old a probe may be and still be reported.
//
// Beyond it the API answers `healthy: null` — "nobody has asked lately" — which
// is a different statement from "unhealthy" and the one an operator needs to
// know a probe has stopped running. A generous window on purpose: reporting a
// provider as unknown because a probe was ten seconds late would train readers
// to ignore the field.
const providerHealthFreshness = 5 * time.Minute

// maxDescriptorBytes bounds a posted descriptor. Large enough for the real
// ones (the DeepWiki descriptor is ~28KB) and small enough that this route
// cannot be used to write arbitrary volume into the database.
const maxDescriptorBytes = 1 << 20

// admissionPlanePresent reports whether migration 0107 has been applied.
//
// WHY A CAPABILITY CHECK AND NOT AN ERROR HANDLER. A deployment that has not
// run 0107 has no admission plane, and that is a DIFFERENT state from "the
// query failed" — it is exactly the state these routes refused for, and the
// recorded reason still describes it. Catching the error instead would answer
// 500 to an operator whose only problem is an unapplied migration, and a 500
// tells them nothing about which one.
//
// It is asked per request rather than cached: a deployment can migrate while
// running, and a cached "absent" would keep refusing after the tables arrived.
// to_regclass answers NULL for a name that does not resolve, without raising.
func (h *Handler) admissionPlanePresent(ctx context.Context) bool {
	var present bool
	if err := h.pool.QueryRow(ctx,
		`SELECT to_regclass('provider_hub.provider_admitted_revision') IS NOT NULL`,
	).Scan(&present); err != nil {
		return false
	}
	return present
}

// ErrProviderNotRegistered reports a revoke for a provider nobody registered.
var ErrProviderNotRegistered = errors.New("provider is not registered for this project")

// serviceDescriptorRow is one row of the listing.
//
// The field names are the ones the admin client already reads — project_id,
// provider_name, service_location_url, healthy — so the page needs no reshaping.
// What changed is that every one of them now comes from storage, and `healthy`
// is a pointer so it can be null.
type serviceDescriptorRow struct {
	ProjectID          int64   `json:"project_id"`
	ProviderName       string  `json:"provider_name"`
	ServiceLocationURL string  `json:"service_location_url"`
	Healthy            *bool   `json:"healthy"`
	Status             string  `json:"status"`
	Reason             string  `json:"reason"`
	ManifestDigest     *string `json:"published_manifest_digest"`
}

// listServiceDescriptors reads every registered provider with its latest
// admission and its health projection.
//
// A LEFT JOIN on both, deliberately: a provider that is registered and has no
// admitted revision yet is a real state an operator must be able to see, and an
// inner join would hide exactly the rows they are looking for.
func (h *Handler) listServiceDescriptors(ctx context.Context) ([]serviceDescriptorRow, error) {
	rows, err := h.pool.Query(ctx, `
SELECT o.project_id,
       o.provider_id,
       o.origin,
       -- NULL unless a probe reported inside the window. The comparison is
       -- against the DATABASE clock, not the caller's: two servers with
       -- skewed clocks must not disagree about whether a reading is fresh.
       CASE WHEN p.observed_at IS NOT NULL
             AND p.observed_at > clock_timestamp() - ($1::bigint * interval '1 second')
            THEN p.healthy END,
       COALESCE(r.status, 'unregistered'),
       COALESCE(r.reason, ''),
       r.manifest_digest
  FROM provider_hub.provider_origin_registration AS o
  LEFT JOIN provider_hub.provider_health_projection AS p
         ON p.project_id = o.project_id AND p.provider_id = o.provider_id
  LEFT JOIN LATERAL (
        SELECT status, reason, manifest_digest
          FROM provider_hub.provider_admitted_revision
         WHERE project_id = o.project_id AND provider_id = o.provider_id
         -- the revision in force first; see providerhub.LatestAdmission
         ORDER BY (status = 'active') DESC, admitted_at DESC, (status = 'revoked') DESC, revision_id DESC
         LIMIT 1
  ) AS r ON TRUE
 ORDER BY o.project_id, o.provider_id`, int64(providerHealthFreshness.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("list service descriptors: %w", err)
	}
	defer rows.Close()

	out := make([]serviceDescriptorRow, 0)
	for rows.Next() {
		var row serviceDescriptorRow
		if err := rows.Scan(&row.ProjectID, &row.ProviderName, &row.ServiceLocationURL,
			&row.Healthy, &row.Status, &row.Reason, &row.ManifestDigest); err != nil {
			return nil, fmt.Errorf("scan service descriptor: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

type registerDescriptorRequest struct {
	ProviderName       string          `json:"provider_name"`
	ServiceLocationURL string          `json:"service_location_url"`
	Descriptor         json.RawMessage `json:"descriptor"`
}

type registerDescriptorResponse struct {
	AdmittedProviderRevision string `json:"admitted_provider_revision"`
	PublishedManifestDigest  string `json:"published_manifest_digest"`
	Status                   string `json:"status"`
	Reason                   string `json:"reason"`
}

// admissionInactiveReason is what every registration records, and it is the
// same sentence the response carries.
//
// One string, because an operator reading the row and an operator reading the
// API answer must not be told two different things — the same rule
// ServiceDescriptorsUnavailableReason follows.
// registerDescriptor is the administration surface's write: the same
// origin + manifest + inactive revision the facades' registrar files at
// boot (internal/providerhost/registrar), through the one implementation in
// internal/providerhub.
func (h *Handler) registerDescriptor(
	ctx context.Context, projectID int64, actor string, req registerDescriptorRequest,
) (registerDescriptorResponse, error) {
	admitted, err := providerhub.Register(ctx, h.pool, providerhub.Registration{
		ProjectID:  projectID,
		ProviderID: req.ProviderName,
		Origin:     req.ServiceLocationURL,
		Manifest:   []byte(req.Descriptor),
		Actor:      actor,
	})
	if err != nil {
		return registerDescriptorResponse{}, err
	}
	return registerDescriptorResponse{
		AdmittedProviderRevision: admitted.RevisionID,
		PublishedManifestDigest:  admitted.ManifestDigest,
		Status:                   admitted.Status,
		Reason:                   admitted.Reason,
	}, nil
}

func (h *Handler) revokeDescriptor(
	ctx context.Context, projectID int64, providerName, actor, reason string,
) error {
	tag, err := h.pool.Exec(ctx, `
UPDATE provider_hub.provider_admitted_revision
   SET status = 'revoked',
       reason = $4,
       revoked_at = clock_timestamp(),
       revoked_by = $3
 WHERE project_id = $1 AND provider_id = $2 AND status <> 'revoked'`,
		projectID, providerName, actor, reason)
	if err != nil {
		return fmt.Errorf("revoke revisions: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Distinguished from success: a revoke that matched nothing usually
		// means a misspelt provider name, and reporting it as done sends the
		// operator away believing they have turned something off.
		var exists bool
		if err := h.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM provider_hub.provider_origin_registration
                WHERE project_id = $1 AND provider_id = $2)`,
			projectID, providerName).Scan(&exists); err != nil {
			return fmt.Errorf("check registration: %w", err)
		}
		if !exists {
			return ErrProviderNotRegistered
		}
	}
	return nil
}

// providerActor names who is acting, for the audit columns.
//
// Falls back to a marker rather than to an empty string: the columns are NOT
// NULL and non-empty by constraint, and a blank actor in an audit trail is
// worse than an explicit "unknown" because it reads as a missing value nobody
// chose.
func providerActor(r *http.Request) string {
	if user, ok := auth.UserFromContext(r.Context()); ok {
		if user.Email != "" {
			return user.Email
		}
		if user.ID != "" {
			return user.ID
		}
	}
	return "unknown"
}

func projectIDFromPath(r *http.Request) (int64, error) {
	raw := chi.URLParam(r, "projectID")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("project id %q is not a positive integer", raw)
	}
	return id, nil
}

func decodeRegisterRequest(r *http.Request) (registerDescriptorRequest, error) {
	var req registerDescriptorRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDescriptorBytes+1))
	if err != nil {
		return req, errors.New("the request body could not be read")
	}
	if len(body) > maxDescriptorBytes {
		return req, fmt.Errorf("the descriptor exceeds the %d byte limit", maxDescriptorBytes)
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return req, errors.New("the request body is not valid JSON")
	}
	req.ProviderName = strings.TrimSpace(req.ProviderName)
	req.ServiceLocationURL = strings.TrimSpace(req.ServiceLocationURL)
	if req.ProviderName == "" {
		return req, errors.New("provider_name is required")
	}
	// The origin is checked by a database constraint too (0107, widened by
	// 0171). Rejecting it here as well gives the caller a message about their
	// input rather than a 500 carrying a constraint name. grpcs:// is the
	// provider's gRPC listener, the origin the facades' registrar records
	// (#1359), so an operator re-registering what the listing shows is
	// accepted.
	rest, ok := "", false
	for _, scheme := range []string{"http://", "https://", "grpcs://"} {
		if after, found := strings.CutPrefix(req.ServiceLocationURL, scheme); found {
			rest, ok = after, true
			break
		}
	}
	if !ok {
		return req, errors.New("service_location_url must be an absolute http(s) or grpcs origin")
	}
	if rest == "" || strings.Contains(rest, "/") {
		return req, errors.New("service_location_url must be an origin with no path")
	}
	if len(req.Descriptor) == 0 {
		return req, errors.New("descriptor is required")
	}
	return req, nil
}
