package repos

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	indexscheduleapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

func TestCurrentIndexScheduleCatalogUsesGeneratedQueryInterfaces(t *testing.T) {
	t.Parallel()

	shared := &scriptedCurrentIndexScheduleSharedQueries{
		projectIDs: []int32{2, 7},
	}
	projectQueries := &scriptedCurrentIndexScheduleProjectQueries{
		toolkits: []sqlcgen.ListCurrentIndexScheduleToolkitsRow{{
			ID:   19,
			Type: "github",
			IndexesMeta: []byte(`{"docs":{"schedules":{"11":{
				"cron":"0 3 * * *","enabled":true,"created_by":11,
				"timezone":"UTC","last_run":"2026-07-27T03:00:00+00:00"
			}}}}`),
		}},
	}
	projectStore := &currentScheduleProjectStore{
		scriptedExecutor: &scriptedExecutor{},
	}
	catalog, err := newCurrentIndexScheduleCatalog(
		shared,
		projectStore,
		func(sqlExecutor) (currentIndexScheduleProjectQueries, error) {
			return projectQueries, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	projectIDs, err := catalog.ListProjectPage(context.Background(), 1, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(projectIDs, []int64{2, 7}) {
		t.Fatalf("project IDs=%v", projectIDs)
	}
	if !reflect.DeepEqual(
		shared.params,
		[]sqlcgen.ListCurrentIndexScheduleProjectsParams{{
			AfterProjectID: 1,
			PageLimit:      8,
		}},
	) {
		t.Fatalf("project query params=%+v", shared.params)
	}

	toolkits, err := catalog.ListToolkitSchedulePage(
		context.Background(),
		7,
		0,
		8,
	)
	if err != nil {
		t.Fatal(err)
	}
	if projectStore.projectID != 7 ||
		len(toolkits) != 1 ||
		toolkits[0].ToolkitID != 19 ||
		len(toolkits[0].Candidates) != 1 ||
		toolkits[0].Candidates[0].ScheduleUserID != 11 {
		t.Fatalf("toolkits=%+v project=%d", toolkits, projectStore.projectID)
	}
	if !reflect.DeepEqual(
		projectQueries.listParams,
		[]sqlcgen.ListCurrentIndexScheduleToolkitsParams{{
			AfterToolkitID: 0,
			PageLimit:      8,
		}},
	) {
		t.Fatalf("toolkit query params=%+v", projectQueries.listParams)
	}
}

func TestCurrentToolkitScheduleCandidatesAreDeterministicAndPreserveInvalidRows(t *testing.T) {
	indexesMeta := []byte(`{
	    "wiki": {
	      "schedules": {
	        "12": {
	          "cron": "0 4 * * *",
	          "enabled": true,
	          "credentials": {"private": true, "elitea_title": "personal"},
	          "created_by": 12,
	          "timezone": "Europe/Kyiv",
	          "last_run": "2026-07-27T01:00:00+00:00"
	        }
	      }
	    },
	    "docs": {
	      "schedules": {
	        "invalid": {"enabled": true},
	        "-1": {
	          "cron": "0 3 * * *",
	          "enabled": true,
	          "credentials": null,
	          "created_by": 11,
	          "timezone": "UTC",
	          "last_run": "2026-07-27T03:00:00+00:00"
	        }
	      }
	    }
	}`)
	first, err := currentToolkitScheduleCandidates(7, 9, "github", indexesMeta)
	if err != nil {
		t.Fatal(err)
	}
	second, err := currentToolkitScheduleCandidates(7, 9, "github", indexesMeta)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("candidate order drifted:\n%s\n%s", firstJSON, secondJSON)
	}
	if len(first) != 3 ||
		first[0].IndexMetaID != "docs" || first[0].ScheduleUserID != -1 ||
		first[1].IndexMetaID != "docs" || first[1].ScheduleUserID != 0 ||
		first[2].IndexMetaID != "wiki" || first[2].ScheduleUserID != 12 {
		t.Fatalf("unexpected candidates: %+v", first)
	}
	if first[2].Schedule.Credentials == nil ||
		first[2].Schedule.Credentials.Private == nil ||
		!*first[2].Schedule.Credentials.Private {
		t.Fatalf("nested private schedule credentials were not preserved: %+v", first[2])
	}
}

type scriptedCurrentIndexScheduleSharedQueries struct {
	projectIDs []int32
	err        error
	params     []sqlcgen.ListCurrentIndexScheduleProjectsParams
}

func (queries *scriptedCurrentIndexScheduleSharedQueries) ListCurrentIndexScheduleProjects(
	_ context.Context,
	params sqlcgen.ListCurrentIndexScheduleProjectsParams,
) ([]int32, error) {
	queries.params = append(queries.params, params)
	return queries.projectIDs, queries.err
}

type scriptedCurrentIndexScheduleProjectQueries struct {
	toolkits   []sqlcgen.ListCurrentIndexScheduleToolkitsRow
	listErr    error
	listParams []sqlcgen.ListCurrentIndexScheduleToolkitsParams
}

func (queries *scriptedCurrentIndexScheduleProjectQueries) ListCurrentIndexScheduleToolkits(
	_ context.Context,
	params sqlcgen.ListCurrentIndexScheduleToolkitsParams,
) ([]sqlcgen.ListCurrentIndexScheduleToolkitsRow, error) {
	queries.listParams = append(queries.listParams, params)
	return queries.toolkits, queries.listErr
}

func (*scriptedCurrentIndexScheduleProjectQueries) LockCurrentIndexScheduleToolkitMeta(
	context.Context,
	int32,
) ([]byte, error) {
	return nil, nil
}

func (*scriptedCurrentIndexScheduleProjectQueries) UpdateCurrentIndexScheduleToolkitMeta(
	context.Context,
	sqlcgen.UpdateCurrentIndexScheduleToolkitMetaParams,
) (int64, error) {
	return 0, nil
}

func TestSameCurrentScheduleComparesNullableCredentialsByValue(t *testing.T) {
	private := true
	left := indexscheduleapp.Schedule{
		Cron: "0 3 * * *", Enabled: true, CreatedBy: 11, Timezone: "UTC",
		LastRun: "2026-07-27T03:00:00+00:00",
		Credentials: &indexscheduleapp.Credentials{
			Private: &private, EliteaTitle: "github",
		},
	}
	otherPrivate := true
	right := left
	right.Credentials = &indexscheduleapp.Credentials{
		Private: &otherPrivate, EliteaTitle: "github",
	}
	if !sameCurrentSchedule(left, right) {
		t.Fatal("equal credential values were treated as different pointers")
	}
	otherPrivate = false
	if sameCurrentSchedule(left, right) {
		t.Fatal("different private values were treated as equal")
	}
	right.Credentials.Private = nil
	if sameCurrentSchedule(left, right) {
		t.Fatal("null and concrete private values were treated as equal")
	}
}
