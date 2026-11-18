package eliteacore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// The yank of a listing version (ADR-0051 decision 4). Every project that holds
// the version shows a banner (the origin says the version is yanked) and its
// people get one notification. The agents keep running on their snapshot unless
// a platform administrator also suspends them, with the ADR-0041 reason
// listing_yanked. A restore does not resume a suspension: resuming is the
// administrator's decision, made in the kill switch.

// SuspensionReasonListingYanked is the reason an administrator's suspension of
// the agents that hold a yanked version starts with.
const SuspensionReasonListingYanked = "listing_yanked"

// ListingSuspender suspends one agent, as an administrator would in the kill
// switch.
type ListingSuspender interface {
	SuspendAgent(ctx context.Context, actorUserID, projectID, applicationID int64, reason string) error
}

// WithListingSuspender wires the kill switch into the yank.
func WithListingSuspender(suspender ListingSuspender) Option {
	return func(handler *Handler) {
		if suspender != nil {
			handler.suspender = suspender
		}
	}
}

type serviceSuspender struct{ service *safety.Service }

// NewListingSuspender adapts the kill switch service. A nil service answers
// nil, which leaves the yank without suspension.
func NewListingSuspender(service *safety.Service) ListingSuspender {
	if service == nil {
		return nil
	}
	return serviceSuspender{service: service}
}

// SuspendAgent suspends the agent at agent scope as a platform administrator.
// An agent that is already suspended is not an error.
func (s serviceSuspender) SuspendAgent(ctx context.Context, actorUserID, projectID, applicationID int64, reason string) error {
	_, err := s.service.Create(ctx, safety.Actor{UserID: actorUserID, PlatformAdmin: true}, safety.CreateRequest{
		Target: safety.Target{Scope: safety.ScopeAgent, ProjectID: projectID, ApplicationID: applicationID},
		Reason: reason, ResumeRequires: safety.ResumeAnyAdmin,
	})
	var already *safety.AlreadySuspendedError
	if errors.As(err, &already) {
		return nil
	}
	return err
}

// announceYank tells the people of every holder of a yanked version, and
// suspends the installed agents when asked. It logs and carries on: the yank is
// already recorded, and a notification that fails must not undo it.
func (h *Handler) announceYank(ctx context.Context, listingID, versionID int64, reason string, actor int64, suspend bool) {
	holders, err := marketplace.HoldersOf(ctx, h.pool, listingID, versionID)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace yank: list the holders failed", "listing_id", listingID, "error", err)
		return
	}
	version, err := marketplace.LoadVersion(ctx, h.pool, listingID, versionID)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace yank: read the version failed", "listing_id", listingID, "error", err)
		return
	}
	why := strings.TrimSpace(reason)
	if why != "" {
		why = " Reason: " + why
	}
	for _, holder := range holders {
		message := fmt.Sprintf("%s %s was yanked. The agent keeps running on its snapshot; upgrade it to another version.%s",
			version.ListingName, version.Name, why)
		if holder.Mode == marketplace.ModeFork {
			message = fmt.Sprintf("%s %s, the version this fork stands on, was yanked.%s", version.ListingName, version.Name, why)
		}
		meta := map[string]any{
			"message": message, "project_id": holder.ProjectID, "application_id": holder.EntityID,
			"listing_id": listingID, "version_id": versionID, "mode": holder.Mode,
		}
		for _, person := range marketplace.OwnersOf(ctx, h.pool, holder.ProjectID, holder.EntityID, holder.InstalledBy) {
			key := fmt.Sprintf("yank-%d-%d-%d-%d-%d", listingID, versionID, holder.ProjectID, holder.EntityID, person)
			if err := marketplace.Notify(ctx, h.pool, key, holder.ProjectID, person, marketplace.NotifyVersionYanked, meta); err != nil {
				slog.WarnContext(ctx, "marketplace yank: notify failed", "project_id", holder.ProjectID, "error", err)
			}
		}
		if !suspend || holder.Mode != marketplace.ModeInstall {
			continue
		}
		text := fmt.Sprintf("%s: %s %s was yanked from the Marketplace.%s", SuspensionReasonListingYanked, version.ListingName, version.Name, why)
		if err := h.suspender.SuspendAgent(ctx, actor, holder.ProjectID, holder.EntityID, text); err != nil {
			slog.ErrorContext(ctx, "marketplace yank: suspend the agent failed",
				"project_id", holder.ProjectID, "application_id", holder.EntityID, "error", err)
		}
	}
}
