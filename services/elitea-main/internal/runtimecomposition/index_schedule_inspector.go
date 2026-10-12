package runtimecomposition

import (
	"context"
	"errors"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
	indexscheduleapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexschedule"
)

type currentIndexScheduleInspector struct {
	exact currentIndexExactFinder
}

func newCurrentIndexScheduleInspector(
	exact currentIndexExactFinder,
) (*currentIndexScheduleInspector, error) {
	if exact == nil {
		return nil, errors.New("exact current index schedule inspector is required")
	}
	return &currentIndexScheduleInspector{exact: exact}, nil
}

func (inspector *currentIndexScheduleInspector) InspectScheduledIndex(
	ctx context.Context,
	candidate indexscheduleapp.Candidate,
	accessUserID int64,
	toolkit indexingapp.CurrentToolkitSnapshot,
) (indexscheduleapp.ScheduledIndex, bool, error) {
	item, found, err := inspector.exact.FindSnapshot(
		ctx,
		indexmetaapp.Request{
			ProjectID:   candidate.ProjectID,
			ActorUserID: accessUserID,
			ToolkitID:   candidate.ToolkitID,
		},
		candidate.IndexMetaID,
		toolkit,
	)
	if err != nil || !found {
		return indexscheduleapp.ScheduledIndex{}, found, err
	}
	state, _ := item.Metadata["state"].(string)
	return indexscheduleapp.ScheduledIndex{
		State:         state,
		Configuration: item.Metadata["index_configuration"],
	}, true, nil
}

var _ indexscheduleapp.ScheduledIndexInspector = (*currentIndexScheduleInspector)(nil)
