package eliteacore

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

var _ repos.ListingHirer = (*Handler)(nil)

// HireListingTx is the workforce policy's hire of a Marketplace listing
// (ADR-0051 decision 6, issue 1632). It is the hire of the wizard, run in the
// caller's transaction: there is one hire, with the same checks, whoever asks
// for it. The avatar files are not copied (the policy has no request to copy
// them under), and a hire that would need a person's approval is refused with
// repos.ErrListingHireRefused, leaving the proposal to a person.
func (h *Handler) HireListingTx(ctx context.Context, tx pgx.Tx, request repos.ListingHireRequest) (repos.ListingHireResult, error) {
	if h == nil || h.pool == nil {
		return repos.ListingHireResult{}, fmt.Errorf("marketplace hire: the handler has no database")
	}
	bindings := make([]hireBindingBody, 0, len(request.Bindings))
	for _, binding := range request.Bindings {
		bindings = append(bindings, hireBindingBody{
			RequirementKey: binding.RequirementKey, ToolkitID: strconv.FormatInt(binding.ToolkitID, 10),
		})
	}
	result, failure := h.performHire(ctx, tx, hireInput{
		ProjectIDText: strconv.FormatInt(request.ProjectID, 10), ListingID: request.ListingID, VersionID: request.VersionID,
		Mode: request.Mode, Bindings: bindings, ActorID: request.ActorUserID, SkipAvatar: true, Automatic: true,
	})
	if failure != nil {
		if failure.status >= http.StatusInternalServerError {
			return repos.ListingHireResult{}, fmt.Errorf("marketplace hire: the policy's hire failed with status %d", failure.status)
		}
		slog.InfoContext(ctx, "marketplace hire: the policy leaves the hire to a person",
			"project_id", request.ProjectID, "listing_id", request.ListingID, "status", failure.status)
		return repos.ListingHireResult{}, repos.ErrListingHireRefused
	}
	applicationID, err := strconv.ParseInt(fmt.Sprint(result["application_id"]), 10, 64)
	if err != nil || applicationID <= 0 {
		return repos.ListingHireResult{}, fmt.Errorf("marketplace hire: the hire answered no agent id")
	}
	return repos.ListingHireResult{ApplicationID: applicationID}, nil
}
