package repos

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

func TestApplicationsRepoPostgres_ListExtendedFiltersAndFolderIsolation(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "first@example.com")
	seedUser(t, pool, 2, "second@example.com")
	alpha := tagTestApplication(t, repo, pool, "Alpha", "red")
	beta := tagTestApplication(t, repo, pool, "Beta", "red")
	gamma := createTestApplication(t, repo, "Gamma", 1, &applications.Version{Name: "base", AgentType: "openai"})
	published, err := repo.CreateVersion(ctx, "1", alpha.ID, applications.Version{Name: "published", Status: "published", AuthorID: 2, AgentType: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CreateVersion(ctx, "1", gamma.ID, applications.Version{Name: "pipeline", AuthorID: 2, AgentType: "pipeline"}); err != nil {
		t.Fatal(err)
	}
	var blue int
	if err = pool.QueryRow(ctx, `INSERT INTO p_1.tags(name) VALUES('blue') RETURNING id`).Scan(&blue); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO p_1.application_version_tag_association(version_id,tag_id) VALUES($1,$2)`, published.ID, blue); err != nil {
		t.Fatal(err)
	}
	for _, like := range []struct {
		id   string
		user int
		date string
	}{{alpha.ID, 1, "2026-01-10"}, {alpha.ID, 2, "2026-01-11"}, {beta.ID, 2, "2026-02-10"}, {gamma.ID, 1, "2026-03-10"}} {
		if _, err = pool.Exec(ctx, `INSERT INTO p_1.social_likes(entity_name,entity_id,user_id,created_at) VALUES('application',$1,$2,$3::timestamp)`, like.id, like.user, like.date); err != nil {
			t.Fatal(err)
		}
	}
	number := func(id string) int32 {
		n, err := strconv.ParseInt(id, 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		return int32(n)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	offset := 1
	for _, tc := range []struct {
		name  string
		req   applications.ListRequest
		names []string
		total int
	}{
		{"all types", applications.ListRequest{}, []string{"Alpha", "Beta", "Gamma"}, 3},
		{"classic excludes mixed", applications.ListRequest{AgentsType: "classic"}, []string{"Alpha", "Beta"}, 2},
		{"pipeline includes mixed", applications.ListRequest{AgentsType: "pipeline"}, []string{"Gamma"}, 1},
		{"AND tags across versions", applications.ListRequest{Tags: "red,blue"}, []string{"Alpha"}, 1},
		{"author any version", applications.ListRequest{AuthorID: 2}, []string{"Alpha", "Gamma"}, 2},
		{"status any version", applications.ListRequest{Statuses: []string{"published"}}, []string{"Alpha"}, 1},
		{"IDs with page", applications.ListRequest{IDs: []int32{number(alpha.ID), number(gamma.ID)}, PageSize: 1}, []string{"Alpha"}, 2},
		{"exact offset", applications.ListRequest{PageSize: 2, Offset: &offset}, []string{"Beta", "Gamma"}, 3},
		{"actor likes", applications.ListRequest{MyLiked: true}, []string{"Alpha", "Gamma"}, 2},
		{"trend period", applications.ListRequest{TrendStart: &start, TrendEnd: &end}, []string{"Alpha"}, 1},
		{"without tags", applications.ListRequest{WithoutTags: true}, []string{"Gamma"}, 1},
		{"combined", applications.ListRequest{Tags: "red", AuthorID: 2, Statuses: []string{"published"}, MyLiked: true, TrendStart: &start, TrendEnd: &end}, []string{"Alpha"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.ProjectID = "1"
			tc.req.SortBy = "name"
			tc.req.SortOrder = "asc"
			result, err := repo.List(ctx, tc.req)
			if err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for _, app := range result.Rows {
				names = append(names, app.Name)
			}
			if result.Total != tc.total || !reflect.DeepEqual(names, tc.names) {
				t.Fatalf("total=%d names=%v want=%d/%v", result.Total, names, tc.total, tc.names)
			}
		})
	}
	likes, err := repo.List(ctx, applications.ListRequest{ProjectID: "1", SortBy: "likes", SortOrder: "desc"})
	if err != nil {
		t.Fatal(err)
	}
	if likes.Rows[0].ID != alpha.ID {
		t.Fatal("likes sort did not place two likes first")
	}
	if _, err = pool.Exec(ctx, `CREATE TABLE p_1.entity_folders(id integer PRIMARY KEY);
 CREATE TABLE p_1.social_folder_items(folder_id integer,entity text,entity_id integer);
 CREATE TABLE p_1.folder_access_overrides(folder_id integer,user_id integer,access_level text);
 INSERT INTO p_1.entity_folders VALUES(1);
 INSERT INTO p_1.folder_access_overrides VALUES(1,1,'no_access');`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO p_1.social_folder_items VALUES(1,'agent',$1),(1,'pipeline',$2)`, beta.ID, gamma.ID); err != nil {
		t.Fatal(err)
	}
	hidden, err := repo.List(ctx, applications.ListRequest{ProjectID: "1", PageSize: 1, IDs: []int32{number(alpha.ID), number(beta.ID), number(gamma.ID)}})
	if err != nil {
		t.Fatal(err)
	}
	if hidden.Total != 1 || len(hidden.Rows) != 1 || hidden.Rows[0].ID != alpha.ID {
		t.Fatalf("folder exclusion total/page=%+v", hidden)
	}
	otherCtx := auth.ContextWithUser(ctx, auth.User{ID: "2", UserID: "2"})
	other, err := repo.List(otherCtx, applications.ListRequest{ProjectID: "1", MyLiked: true, SortBy: "name", SortOrder: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	if other.Total != 2 || other.Rows[0].ID != alpha.ID || other.Rows[1].ID != beta.ID {
		t.Fatalf("other actor visibility or likes=%+v", other)
	}
}
