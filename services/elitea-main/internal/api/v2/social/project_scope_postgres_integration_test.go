package social_test

// The social routes that carry e-mail addresses or per-project reads must not
// reach past the project the caller belongs to.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// Users: 1 and 2 belong to project 7; 3 belongs to project 8 only; 4 belongs to
// nothing. Every user has a social profile, as every account on the platform
// does, so a listing that is not scoped to the project returns all four.
func prepareProjectScopeDatabase(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
CREATE SCHEMA centry;
CREATE TABLE centry.project (id integer PRIMARY KEY, create_success boolean NOT NULL DEFAULT true, suspended boolean NOT NULL DEFAULT false);
INSERT INTO centry.project (id) VALUES (7), (8);
CREATE TABLE public.auth_core__user (id integer PRIMARY KEY, email text UNIQUE, name text, last_login timestamp, suspended boolean NOT NULL DEFAULT false);
INSERT INTO public.auth_core__user (id, email, name) VALUES
    (1, 'owner@seven.example', 'Owner'),
    (2, 'member@seven.example', 'Member'),
    (3, 'foreign-canary@eight.example', 'Foreign'),
    (4, 'loner-canary@nowhere.example', 'Loner'),
    (5, 'system_user_7@centry.user', 'Project system');
CREATE TABLE public.auth_core__role (id integer PRIMARY KEY, name text NOT NULL, mode text NOT NULL);
CREATE TABLE public.auth_core__user_role (id serial PRIMARY KEY, user_id integer NOT NULL, role_id integer NOT NULL);
CREATE TABLE public.auth_core__project_user_role (
    id serial PRIMARY KEY, project_id integer NOT NULL, user_id integer NOT NULL, role_id integer NOT NULL DEFAULT 1);
INSERT INTO public.auth_core__project_user_role (project_id, user_id) VALUES (7, 1), (7, 2), (7, 5), (8, 3);
CREATE TABLE centry.social_users (
    id serial PRIMARY KEY, user_id integer NOT NULL UNIQUE, avatar varchar, description varchar,
    personalization jsonb, default_context_management jsonb, default_summarization jsonb);
INSERT INTO centry.social_users (user_id, avatar) VALUES (1, 'a1'), (2, 'a2'), (3, 'a3'), (4, 'a4'), (5, 'a5');
CREATE SCHEMA p_7;
CREATE TABLE p_7.social_likes (id serial PRIMARY KEY, entity_name varchar NOT NULL, entity_id integer NOT NULL, user_id integer NOT NULL);
-- The foreign user out-likes everybody; an unscoped ranking puts them first.
INSERT INTO p_7.social_likes (entity_name, entity_id, user_id) VALUES
    ('application', 1, 3), ('application', 2, 3), ('application', 3, 3), ('application', 1, 4),
    ('application', 1, 1), ('application', 2, 2);`); err != nil {
		t.Fatal(err)
	}
}

func projectScopeRouter(pool *pgxpool.Pool) http.Handler {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Test-Actor")
			ctx := auth.ContextWithUser(r.Context(), auth.User{ID: id, UserID: id})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	router.Mount("/api/v2/social", handler.NewHandler(pool).Routes())
	return router
}

func projectScopeGet(router http.Handler, actor, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("X-Test-Actor", actor)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestSocialAuthorListingIsLimitedToTheProjectsMembers(t *testing.T) {
	pool := newCurrentAuthorsPostgresPool(t)
	prepareProjectScopeDatabase(t, pool)
	router := projectScopeRouter(pool)

	recorder := projectScopeGet(router, "1", "/api/v2/social/authors/7")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body)
	}
	var authors []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &authors); err != nil {
		t.Fatal(err)
	}
	var emails []string
	for _, author := range authors {
		emails = append(emails, author["email"].(string))
	}
	if got := strings.Join(emails, ","); got != "owner@seven.example,member@seven.example" {
		t.Fatalf("project 7 listed %q, want its two members and nobody else (not the project system user either)", got)
	}
	for _, canary := range []string{"foreign-canary@eight.example", "loner-canary@nowhere.example"} {
		if strings.Contains(recorder.Body.String(), canary) {
			t.Fatalf("the listing leaked %s", canary)
		}
	}

	// A member of another project, and a user with no project, are refused
	// before any row is read.
	for _, actor := range []string{"3", "4"} {
		refused := projectScopeGet(router, actor, "/api/v2/social/authors/7")
		if refused.Code != http.StatusForbidden || strings.Contains(refused.Body.String(), "@") {
			t.Fatalf("actor %s: status %d body %s, want a bare 403", actor, refused.Code, refused.Body)
		}
	}
	// Project 8's own member sees only project 8's people.
	own := projectScopeGet(router, "3", "/api/v2/social/authors/8")
	if own.Code != http.StatusOK || !strings.Contains(own.Body.String(), "foreign-canary@eight.example") ||
		strings.Contains(own.Body.String(), "@seven.example") {
		t.Fatalf("project 8 listing: status %d body %s", own.Code, own.Body)
	}
}

func TestSocialTrendingAuthorsRankOnlyTheProjectsMembers(t *testing.T) {
	pool := newCurrentAuthorsPostgresPool(t)
	prepareProjectScopeDatabase(t, pool)
	router := projectScopeRouter(pool)

	recorder := projectScopeGet(router, "1", "/api/v2/social/trending_authors/prompt_lib/7")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()
	for _, canary := range []string{"foreign-canary@eight.example", "loner-canary@nowhere.example", "system_user_7@centry.user"} {
		if strings.Contains(body, canary) {
			t.Fatalf("the ranking leaked %s: %s", canary, body)
		}
	}
	if !strings.Contains(body, "owner@seven.example") || !strings.Contains(body, "member@seven.example") {
		t.Fatalf("the ranking lost the project's own members: %s", body)
	}
	if refused := projectScopeGet(router, "3", "/api/v2/social/trending_authors/prompt_lib/7"); refused.Code != http.StatusForbidden {
		t.Fatalf("a member of another project got %d", refused.Code)
	}
}

// GET /social/author is the caller's own profile whatever the table holds for
// anybody else.
func TestSocialGetAuthorIsAlwaysTheCallersOwnProfile(t *testing.T) {
	pool := newCurrentAuthorsPostgresPool(t)
	prepareProjectScopeDatabase(t, pool)
	router := projectScopeRouter(pool)

	for actor, email := range map[string]string{
		"1": "owner@seven.example",
		"3": "foreign-canary@eight.example",
	} {
		recorder := projectScopeGet(router, actor, "/api/v2/social/author")
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("actor %s: %v: %s", actor, err, recorder.Body)
		}
		if recorder.Code != http.StatusOK || body["email"] != email {
			t.Fatalf("actor %s: status %d, email %v; want their own %s", actor, recorder.Code, body["email"], email)
		}
		for _, other := range []string{"member@seven.example", "loner-canary@nowhere.example"} {
			if actor != "2" && strings.Contains(recorder.Body.String(), other) && other != email {
				t.Fatalf("actor %s read somebody else's e-mail %s", actor, other)
			}
		}
	}
}

// TestSocialAuthorListingOfThePublicProjectShowsOnlyTheCallersEmail: sign-up
// enrolment can put every user in the public project, so its listing would be
// the whole platform's e-mail directory. It lists the members, and only the
// caller's own row carries an address.
func TestSocialAuthorListingOfThePublicProjectShowsOnlyTheCallersEmail(t *testing.T) {
	t.Setenv("PUBLIC_PROJECT_ID", "8")
	pool := newCurrentAuthorsPostgresPool(t)
	prepareProjectScopeDatabase(t, pool)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO public.auth_core__project_user_role (project_id, user_id) VALUES (8, 2)`); err != nil {
		t.Fatal(err)
	}
	router := projectScopeRouter(pool)

	recorder := projectScopeGet(router, "3", "/api/v2/social/authors/8")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body)
	}
	var authors []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &authors); err != nil {
		t.Fatal(err)
	}
	if len(authors) != 2 {
		t.Fatalf("listed %d authors, want both members: %s", len(authors), recorder.Body)
	}
	for _, author := range authors {
		email, present := author["email"]
		switch author["id"] {
		case "3":
			if email != "foreign-canary@eight.example" {
				t.Fatalf("the caller's own row lost its e-mail: %v", author)
			}
		default:
			if present {
				t.Fatalf("another member's e-mail is listed in the public project: %v", author)
			}
		}
	}
}
