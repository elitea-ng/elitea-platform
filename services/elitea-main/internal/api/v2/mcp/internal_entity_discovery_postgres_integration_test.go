package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/entitydiscovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/foldervisibility"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestInternalDiscoveryPostgresCountsVisibilityFiltersAndEnvelopes(t *testing.T) {
	t.Setenv("ELITEA_AI_PROJECT_ID", "1")
	pool := newInternalApplicationsPool(t)
	ctx := auth.ContextWithUser(context.Background(), auth.User{ID: "41", UserID: "41"})
	_, err := pool.Exec(ctx, `
 INSERT INTO p_1.applications(id,name,description,owner_id) VALUES
 (10,'Visible agent','agent text',1),(20,'Mixed pipeline','pipeline text',1),(30,'Hidden agent','hidden text',1);
 INSERT INTO p_1.application_versions(id,application_id,name,author_id,agent_type,status) VALUES
 (101,10,'base',41,'openai','draft'),(102,10,'published',42,'openai','published'),
 (201,20,'base',41,'openai','draft'),(202,20,'pipeline',42,'pipeline','published'),(301,30,'base',41,'openai','draft');
 INSERT INTO p_1.skills(id,name,description,owner_id,author_id) VALUES (40,'Skill option','find skill description',1,41);
 INSERT INTO p_1.skill_versions(id,skill_id,name,instructions,author_id) VALUES (401,40,'base','instructions',41);
 ALTER TABLE p_1.tags ALTER COLUMN data TYPE json USING data::json;
 INSERT INTO p_1.tags(id,name,data) VALUES (1,'common','{"color":"blue"}'),(2,'pipeline-only',NULL),(3,'skill-only',NULL),(4,'hidden-only',NULL),(5,'unattached',NULL);
 INSERT INTO p_1.application_version_tag_association(version_id,tag_id) VALUES (101,1),(102,1),(201,1),(201,2),(301,4);
 INSERT INTO p_1.skill_version_tag_association(version_id,tag_id) VALUES (401,1),(401,3);
 INSERT INTO p_1.elitea_tools(id,name,type,owner_id,author_id,settings) VALUES (50,'','github',1,41,'{"elitea_title":"Readable toolkit","token":"secret-canary"}');
 INSERT INTO p_1.configuration(id,project_id,label,elitea_title,type,section,data) VALUES (60,1,'Credential option','credential_option','github','credentials','{"token":"secret-canary"}');
 CREATE TABLE IF NOT EXISTS p_1.entity_folders(id integer PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS p_1.social_folder_items(folder_id integer,entity text,entity_id integer);
 CREATE TABLE IF NOT EXISTS p_1.folder_access_overrides(folder_id integer,user_id integer,access_level text);
 INSERT INTO p_1.social_folder_items(folder_id,entity,entity_id) VALUES (1,'agent',30);
 INSERT INTO p_1.folder_access_overrides(folder_id,user_id,access_level) VALUES (1,41,'no_access');
 INSERT INTO p_1.social_likes(entity_name,entity_id,user_id) VALUES ('application',10,41);
 `)
	if err != nil {
		t.Fatal(err)
	}
	svc := entitydiscovery.New(pool)
	page, err := svc.Tags(ctx, "1", entitydiscovery.Filters{Coverage: "application", Limit: 1000})
	if err != nil || page.Total != 1 || len(page.Rows) != 1 || page.Rows[0].ApplicationCount == nil || *page.Rows[0].ApplicationCount != 1 {
		t.Fatalf("application tags=%+v err=%v", page, err)
	}
	pipeline, err := svc.Tags(ctx, "1", entitydiscovery.Filters{Coverage: "pipeline", Limit: 1000, Statuses: []string{"published"}, AuthorID: 41})
	if err != nil || pipeline.Total != 2 {
		t.Fatalf("mixed entity pipeline=%+v err=%v", pipeline, err)
	}
	skill, err := svc.Tags(ctx, "1", entitydiscovery.Filters{Coverage: "skill", Limit: 1000})
	if err != nil || skill.Total != 2 || skill.Rows[0].SkillCount == nil || *skill.Rows[0].SkillCount != 1 {
		t.Fatalf("skill tags=%+v err=%v", skill, err)
	}
	all, err := svc.Tags(ctx, "1", entitydiscovery.Filters{Coverage: "all", Limit: 1000})
	if err != nil || all.Total != 3 {
		t.Fatalf("all tags=%+v err=%v", all, err)
	}
	for _, tag := range all.Rows {
		if tag.ID == 4 || tag.ID == 5 {
			t.Fatalf("hidden or unattached tag=%+v", tag)
		}
	}
	empty, err := svc.Tags(ctx, "1", entitydiscovery.Filters{Coverage: "application", Limit: 1, Offset: 1})
	if err != nil || empty.Total != 1 || len(empty.Rows) != 0 {
		t.Fatalf("pagination=%+v err=%v", empty, err)
	}
	for _, filters := range []entitydiscovery.Filters{
		{Coverage: "application", Limit: 10, Query: "no-match"}, {Coverage: "pipeline", Limit: 10, MyLiked: true},
		{Coverage: "application", Limit: 10, Search: "no-match"}, {Coverage: "application", Limit: 10, AuthorID: 99},
	} {
		page, err := svc.Tags(ctx, "1", filters)
		if err != nil || page.Total != 0 {
			t.Fatalf("filtered=%+v err=%v", page, err)
		}
	}
	values := url.Values{"entities[]": {"application", "pipeline", "toolkit", "credential", "skill", "tag"}}
	result, err := svc.SearchOptions(ctx, "1", values)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if len(result) != 7 || strings.Contains(string(raw), "secret-canary") || strings.Contains(string(raw), "Hidden agent") || strings.Contains(string(raw), "hidden-only") || !strings.Contains(string(raw), "Readable toolkit") {
		t.Fatalf("result=%s", raw)
	}
	for _, kind := range []string{"application", "pipeline", "skill", "toolkit"} {
		if result[kind].(entitydiscovery.Page[map[string]any]).Total != 1 {
			t.Fatalf("%s result=%v", kind, result[kind])
		}
	}
	if _, ok := result["credential"].(map[string]any)["total"]; ok {
		t.Fatal("credential total changes the current envelope")
	}
	// MCP invokes the same read only after permission and project checks.
	resolver := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{"models.promptlib_shared.search"}}}
	mcpHandler := NewHandler(pool, nil, nil, resolver)
	mcpRequest := httptest.NewRequest(http.MethodPost, "/app/1/mcp/elitea_core/discovery", nil).WithContext(ctx)
	mcpResult := mcpHandler.callInternalDiscoveryTool(mcpRequest, 1, internalDiscoveryTools()[1], map[string]any{"entities": []any{"application", "pipeline", "toolkit", "credential", "skill", "tag"}})
	if mcpResult["isError"] == true || textOf(t, mcpResult) != string(raw) || resolver.calls != 1 {
		t.Fatalf("MCP result=%v permission calls=%d", mcpResult, resolver.calls)
	}
	// REST uses the same service and exact singular-section result.
	h := eliteacore.NewHandler(pool)
	req := httptest.NewRequest(http.MethodGet, "/?"+values.Encode(), nil).WithContext(ctx)
	route := chi.NewRouteContext()
	route.URLParams.Add("projectID", "1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
	rec := httptest.NewRecorder()
	h.SearchOptions(rec, req)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != string(raw) {
		t.Fatalf("REST result=%d %s", rec.Code, rec.Body.String())
	}
	// A different actor does not inherit actor 41's explicit denial.
	actor42 := auth.ContextWithUser(ctx, auth.User{ID: "42", UserID: "42"})
	visible, err := svc.Tags(actor42, "1", entitydiscovery.Filters{Coverage: "application", Limit: 10})
	if err != nil || visible.Total != 2 {
		t.Fatalf("actor isolation=%+v err=%v", visible, err)
	}
	if _, err := svc.Tags(context.Background(), "1", entitydiscovery.Filters{Coverage: "all", Limit: 10}); !errors.Is(err, foldervisibility.ErrIdentity) {
		t.Fatalf("missing identity=%v", err)
	}
	if _, err := svc.Tags(ctx, "999999", entitydiscovery.Filters{Coverage: "all", Limit: 10}); err == nil {
		t.Fatal("missing schema became an empty result")
	}
	// Shared credential options never include private public-project credentials.
	if _, err := pool.Exec(ctx, `SELECT create_tenant_schema('p_2');
 INSERT INTO p_2.configuration(id,project_id,label,elitea_title,type,section) VALUES (70,2,'Private project credential','project_credential','github','credentials');
 INSERT INTO p_1.configuration(id,project_id,label,elitea_title,type,section,shared) VALUES (61,1,'Shared credential','shared_credential','github','credentials',true);`); err != nil {
		t.Fatal(err)
	}
	shared, err := svc.SearchOptions(ctx, "2", url.Values{"entities[]": {"credential"}, "include_shared": {"true"}})
	if err != nil {
		t.Fatal(err)
	}
	sharedBytes, _ := json.Marshal(shared)
	if strings.Contains(string(sharedBytes), "Credential option") || !strings.Contains(string(sharedBytes), "Shared credential") || !strings.Contains(string(sharedBytes), "Private project credential") {
		t.Fatalf("shared options=%s", sharedBytes)
	}
	// Cancellation retains its identity and does not become an empty result.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.Tags(canceled, "1", entitydiscovery.Filters{Coverage: "all", Limit: 10}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled tags=%v", err)
	}
	if _, err := svc.SearchOptions(canceled, "1", values); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled search=%v", err)
	}
	// A partial folder projection fails closed.
	if _, err := pool.Exec(ctx, `ALTER TABLE p_1.social_folder_items RENAME TO unavailable_folder_items`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Tags(ctx, "1", entitydiscovery.Filters{Coverage: "all", Limit: 10}); !errors.Is(err, foldervisibility.ErrPartialProjection) {
		t.Fatalf("partial projection=%v", err)
	}
}
