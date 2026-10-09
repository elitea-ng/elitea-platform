package eliteacore_test

// The author card is keyed by an author id alone, so the handler decides what
// the caller may learn from how the two relate: the e-mail and the counts
// belong to the author and to the people who share a project with them.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const (
	authorA   = "10" // the author: member of projects 1, 7 and 8
	callerB   = "11" // shares project 7 with the author
	callerC   = "12" // member of project 9 only: shares nothing
	callerD   = "13" // member of nothing
	callerE   = "14" // member of the public project (1) only
	authorMax = 100  // eliteacore.authorCountedProjectsMax
)

func authorLookupPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	databaseName := fmt.Sprintf("elitea_author_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quoted+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})
	return pool
}

func authorExec(t *testing.T, pool *pgxpool.Pool, statement string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// seedAuthorTenant creates the tenant tables the counts read and gives the
// author `apps` agents (the last `pipelines` of them pipelines), `toolkits`
// toolkits and `collections` collections in project p.
func seedAuthorTenant(t *testing.T, pool *pgxpool.Pool, project, apps, pipelines, toolkits, collections int) {
	t.Helper()
	schema := fmt.Sprintf("p_%d", project)
	authorExec(t, pool, fmt.Sprintf(`
CREATE SCHEMA %[1]s;
CREATE TABLE %[1]s.applications (id serial PRIMARY KEY, name varchar NOT NULL DEFAULT 'a');
CREATE TABLE %[1]s.application_versions (
    id serial PRIMARY KEY, application_id integer NOT NULL, author_id integer NOT NULL,
    agent_type varchar NOT NULL DEFAULT 'openai');
CREATE TABLE %[1]s.elitea_tools (id serial PRIMARY KEY, author_id integer NOT NULL);`, schema))
	for i := 0; i < apps; i++ {
		agentType := "openai"
		if i >= apps-pipelines {
			agentType = "pipeline"
		}
		authorExec(t, pool, fmt.Sprintf(`
WITH a AS (INSERT INTO %[1]s.applications DEFAULT VALUES RETURNING id)
INSERT INTO %[1]s.application_versions (application_id, author_id, agent_type)
SELECT id, $1, $2 FROM a`, schema), 10, agentType)
	}
	for i := 0; i < toolkits; i++ {
		authorExec(t, pool, fmt.Sprintf(`INSERT INTO %s.elitea_tools (author_id) VALUES (10)`, schema))
	}
	if collections > 0 {
		authorExec(t, pool, fmt.Sprintf(`CREATE TABLE %s.prompt_collections (id serial PRIMARY KEY, author_id integer NOT NULL)`, schema))
		for i := 0; i < collections; i++ {
			authorExec(t, pool, fmt.Sprintf(`INSERT INTO %s.prompt_collections (author_id) VALUES (10)`, schema))
		}
	}
}

func seedAuthorUniverse(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	authorExec(t, pool, `
CREATE SCHEMA centry;
CREATE TABLE public.auth_core__user (id integer PRIMARY KEY, email text UNIQUE, name text);
CREATE TABLE public.auth_core__project_user_role (
    id serial PRIMARY KEY, project_id integer NOT NULL, user_id integer NOT NULL, role_id integer NOT NULL DEFAULT 1,
    UNIQUE (project_id, user_id, role_id));
CREATE TABLE centry.social_users (id serial PRIMARY KEY, user_id integer NOT NULL UNIQUE, avatar varchar, description varchar);
INSERT INTO public.auth_core__user (id, email, name) VALUES
    (10, 'author@example.test', 'Author A'),
    (11, 'colleague@example.test', 'Colleague B'),
    (12, 'stranger@example.test', 'Stranger C'),
    (13, 'loner@example.test', 'Loner D'),
    (14, 'enrolled@example.test', 'Enrolled E');
INSERT INTO centry.social_users (user_id, avatar, description) VALUES (10, 'avatar-a', 'about A');
INSERT INTO public.auth_core__project_user_role (project_id, user_id) VALUES
    (1, 10), (7, 10), (8, 10), (7, 11), (9, 12), (1, 14);`)
	// Project 7: 2 agents (one a pipeline), 1 toolkit, 2 collections.
	seedAuthorTenant(t, pool, 7, 2, 1, 1, 2)
	// Project 8: 1 agent, 3 toolkits, no collections table at all.
	seedAuthorTenant(t, pool, 8, 1, 0, 3, 0)
	// Project 1 is the public project (PUBLIC_PROJECT_ID defaults to 1). Sign-up
	// enrolment can put every user in it, so sharing it makes nobody a
	// colleague; the author's own card still counts it.
	seedAuthorTenant(t, pool, 1, 5, 0, 5, 0)
}

type authorCard map[string]any

func fetchAuthor(t *testing.T, pool *pgxpool.Pool, caller auth.User, target string) (int, authorCard) {
	t.Helper()
	router := chi.NewRouter()
	router.Get("/elitea_core/author/prompt_lib/{authorID}", eliteacore.NewHandler(pool).Author)
	request := httptest.NewRequest(http.MethodGet, "/elitea_core/author/prompt_lib/"+target, nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), caller))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	var card authorCard
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &card); err != nil {
			t.Fatalf("decode %q: %v", recorder.Body.String(), err)
		}
	}
	return recorder.Code, card
}

func session(id string) auth.User {
	return auth.User{ID: id, UserID: id, AuthType: "session"}
}

func (c authorCard) counts() [4]int {
	n := func(key string) int { v, _ := c[key].(float64); return int(v) }
	return [4]int{n("total_applications"), n("total_pipelines"), n("total_toolkits"), n("total_collections")}
}

func TestAuthorCardTellsEachCallerOnlyWhatTheirRelationAllows(t *testing.T) {
	t.Setenv("PUBLIC_PROJECT_ID", "1")
	pool := authorLookupPool(t)
	seedAuthorUniverse(t, pool)

	t.Run("the author sees everything across all their projects", func(t *testing.T) {
		code, card := fetchAuthor(t, pool, session(authorA), authorA)
		if code != http.StatusOK || card["email"] != "author@example.test" {
			t.Fatalf("status %d card %v, want 200 with the author's own e-mail", code, card)
		}
		// projects 1 + 7 + 8: agents 5+1+1, pipelines 0+1+0 (7's second agent), toolkits 5+1+3, collections 2.
		if got, want := card.counts(), [4]int{5 + 1 + 1, 1, 5 + 1 + 3, 2}; got != want {
			t.Fatalf("counts = %v, want %v", got, want)
		}
	})

	t.Run("a colleague gets the e-mail and the counts of the shared project only", func(t *testing.T) {
		code, card := fetchAuthor(t, pool, session(callerB), authorA)
		if code != http.StatusOK || card["email"] != "author@example.test" {
			t.Fatalf("status %d card %v, want 200 with the e-mail", code, card)
		}
		// Project 7 only: one non-pipeline agent, one pipeline, one toolkit, two collections.
		if got, want := card.counts(), [4]int{1, 1, 1, 2}; got != want {
			t.Fatalf("counts = %v, want %v; projects 1 and 8 are not shared", got, want)
		}
		if card["name"] != "Author A" || card["avatar"] != "avatar-a" {
			t.Fatalf("profile fields missing: %v", card)
		}
	})

	for _, stranger := range []struct{ name, id string }{
		{"member elsewhere", callerC}, {"member of nothing", callerD}, {"fellow member of the public project only", callerE},
	} {
		t.Run(stranger.name+" gets no e-mail key and zero counts", func(t *testing.T) {
			code, card := fetchAuthor(t, pool, session(stranger.id), authorA)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if _, present := card["email"]; present {
				t.Fatalf("the e-mail key is present for a caller who shares no project: %v", card)
			}
			if got := card.counts(); got != [4]int{} {
				t.Fatalf("counts = %v, want zeros: the public project is not shared with this caller", got)
			}
			if card["name"] != "Author A" {
				t.Fatalf("the public profile is still served: %v", card)
			}
		})
	}

	t.Run("a token principal is held to the same rule as a session", func(t *testing.T) {
		token := auth.User{ID: callerC, UserID: callerC, TokenID: "5", AuthType: "token"}
		code, card := fetchAuthor(t, pool, token, authorA)
		if _, present := card["email"]; code != http.StatusOK || present {
			t.Fatalf("status %d card %v, want 200 without an e-mail", code, card)
		}
		colleague := auth.User{ID: callerB, UserID: callerB, TokenID: "6", AuthType: "token"}
		if _, card := fetchAuthor(t, pool, colleague, authorA); card["email"] != "author@example.test" {
			t.Fatalf("a token owned by a colleague lost the e-mail: %v", card)
		}
	})

	t.Run("an unknown id, a non-number and a stranger's author are not told apart by status", func(t *testing.T) {
		stranger := session(callerC)
		for _, target := range []string{"999", "0", "-3", "abc", "99999999999999999999", authorA} {
			if code, _ := fetchAuthor(t, pool, stranger, target); code != http.StatusOK {
				t.Fatalf("author %q answered %d, want the same 200 as every other id", target, code)
			}
		}
		if _, card := fetchAuthor(t, pool, stranger, "999"); len(card) != 0 {
			t.Fatalf("an unknown id answered %v, want the empty object", card)
		}
	})

	t.Run("a principal with no owning user is refused", func(t *testing.T) {
		orphan := auth.User{ID: "5", TokenID: "9", AuthType: "token"}
		if code, _ := fetchAuthor(t, pool, orphan, authorA); code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", code)
		}
	})
}

// The work one request can cause is bounded: an author in more projects than
// the cap is counted over the lowest-numbered ones.
func TestAuthorCardCountsAtMostTheCapOfSharedProjects(t *testing.T) {
	pool := authorLookupPool(t)
	authorExec(t, pool, `
CREATE SCHEMA centry;
CREATE TABLE public.auth_core__user (id integer PRIMARY KEY, email text UNIQUE, name text);
CREATE TABLE public.auth_core__project_user_role (
    id serial PRIMARY KEY, project_id integer NOT NULL, user_id integer NOT NULL, role_id integer NOT NULL DEFAULT 1,
    UNIQUE (project_id, user_id, role_id));
CREATE TABLE centry.social_users (id serial PRIMARY KEY, user_id integer NOT NULL UNIQUE, avatar varchar, description varchar);
INSERT INTO public.auth_core__user (id, email, name) VALUES (10, 'author@example.test', 'Author A');`)
	for i := 0; i < authorMax+20; i++ {
		project := 2000 + i
		authorExec(t, pool, `INSERT INTO public.auth_core__project_user_role (project_id, user_id) VALUES ($1, 10)`, project)
		authorExec(t, pool, fmt.Sprintf(`CREATE SCHEMA p_%[1]d;
CREATE TABLE p_%[1]d.applications (id serial PRIMARY KEY);
CREATE TABLE p_%[1]d.application_versions (id serial PRIMARY KEY, application_id integer, author_id integer, agent_type varchar);
CREATE TABLE p_%[1]d.elitea_tools (id serial PRIMARY KEY, author_id integer NOT NULL);
INSERT INTO p_%[1]d.elitea_tools (author_id) VALUES (10);`, project))
	}
	code, card := fetchAuthor(t, pool, session(authorA), authorA)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got := card.counts()[2]; got != authorMax {
		t.Fatalf("toolkits counted = %d, want the cap %d", got, authorMax)
	}
}
