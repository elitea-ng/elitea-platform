package main

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"

	predictapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/predict"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The run brief and the goal check (ADR-0045 decision 11, #1564). The
// service is composed before the runtime (its settle hook and the start
// service take it), but the predict completer is composed after the runtime,
// with the gateway client. deferredRunBriefCompleter bridges the two: until
// the completer is set, every call fails, which reads as a failed brief and
// "not checked".

// errRunBriefNoModelPlane is a call before the gateway completer is set, or
// on a deployment without LLM_GATEWAY_URL.
var errRunBriefNoModelPlane = errors.New("run brief: no LLM gateway is composed")

// deferredRunBriefCompleter is runbrief.Completer over a predict completer
// that is set later.
type deferredRunBriefCompleter struct {
	next atomic.Pointer[predictapi.Completer]
}

// Set wires the predict completer. A nil completer leaves calls failing.
func (d *deferredRunBriefCompleter) Set(completer predictapi.Completer) {
	if completer != nil {
		d.next.Store(&completer)
	}
}

func (d *deferredRunBriefCompleter) Complete(ctx context.Context, request runbrief.CompletionRequest) (string, error) {
	next := d.next.Load()
	if next == nil {
		return "", errRunBriefNoModelPlane
	}
	// Temperature 0: a brief and a check must not change between two
	// identical calls. MaxTokens bounds what one call can cost.
	temperature := 0.0
	maxTokens := request.MaxTokens
	return (*next).Complete(ctx, predictapi.CompletionRequest{
		ProjectID: strconv.FormatInt(request.ProjectID, 10),
		UserID:    strconv.FormatInt(request.UserID, 10),
		Model:     request.Model,
		Messages: []predictapi.Message{
			{Role: "system", Content: request.System},
			{Role: "user", Content: request.User},
		},
		Temperature:   &temperature,
		MaxTokens:     &maxTokens,
		AttributionID: request.AttributionID,
	})
}

// lowTierModels reads the project's low-tier default model from the model
// catalogue (low_tier_default_model_name). Without a catalogue, or without a
// low-tier default, it answers "" and the predict completer takes the
// project's default model.
type lowTierModels struct {
	catalog         predictapi.ModelCatalog
	publicProjectID int32
}

func (m lowTierModels) LowTierModel(ctx context.Context, projectID int64) (string, error) {
	if m.catalog == nil || m.publicProjectID <= 0 || projectID <= 0 || projectID > int64(^uint32(0)>>1) {
		return "", nil
	}
	catalog, err := m.catalog.Get(ctx, configurationapp.CurrentModelCatalogQuery{
		Section: configurationapp.CurrentModelSectionLLM, ProjectID: int32(projectID),
		PublicProjectID: m.publicProjectID, IncludeShared: true,
	})
	if err != nil {
		return "", err
	}
	if catalog.LowTierDefaultModelName == nil || *catalog.LowTierDefaultModelName == "" {
		return "", nil
	}
	for _, item := range catalog.Items {
		if item.Name == *catalog.LowTierDefaultModelName {
			return item.Name, nil
		}
	}
	return "", nil
}

// runBriefSettings reads approvals.run_brief live.
type runBriefSettings struct {
	source platformconfig.UnattendedSettingsSource
}

func (s runBriefSettings) RunBriefEnabled(ctx context.Context) (bool, error) {
	settings, err := s.source.Get(ctx)
	return settings.RunBrief, err
}

// composeRunBriefs builds the run brief service. It answers nil without a
// database.
func composeRunBriefs(
	pool *pgxpool.Pool, catalog predictapi.ModelCatalog, publicProjectID int32, logger *slog.Logger,
) (*runbrief.Service, *deferredRunBriefCompleter) {
	if pool == nil {
		return nil, nil
	}
	completer := &deferredRunBriefCompleter{}
	chains := dbrepos.NewRunChainsRepository(pool)
	service := runbrief.New(completer, lowTierModels{catalog: catalog, publicProjectID: publicProjectID},
		runBriefSettings{source: platformconfig.UnattendedSettingsSource{Pool: pool}}, chains, chains, logger)
	return service, completer
}
