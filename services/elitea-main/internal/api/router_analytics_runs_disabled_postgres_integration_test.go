package api

// The run-level analytics routes honour the Analytics switch
// (platform_config analytics.analytics_enabled), like every other analytics
// route. The switch is read from PostgreSQL, so this case needs a database.
// It SKIPS without ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	v2analytics "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/analytics"
	v2evaluation "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
)

func TestRunAnalyticsRoutesRefuseWhenAnalyticsIsDisabled(t *testing.T) {
	pool := newStatusOKIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA IF NOT EXISTS centry;
CREATE TABLE centry.platform_config (
    section VARCHAR(64) NOT NULL,
    key VARCHAR(128) NOT NULL,
    value JSONB NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_by VARCHAR(255),
    PRIMARY KEY (section, key)
);
INSERT INTO centry.platform_config (section, key, value)
VALUES ('analytics', 'analytics_enabled', 'false'::jsonb)`); err != nil {
		t.Fatalf("seed the analytics switch: %v", err)
	}

	repo := &runAnalyticsRepo{}
	router := NewRouter(RouterConfig{
		Pool:                      pool,
		AuthValidator:             testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:        testPrincipalValidator{},
		AnalyticsRepo:             repo,
		ProjectAccessQuerier:      &memberOfProject{project: "7"},
		ProjectPermissionResolver: fakePermissionResolver{granted: []string{v2analytics.ViewPermission, v2evaluation.PermissionRunRead}, forProject: "7"},
	})
	for _, path := range []string{
		fmt.Sprintf(executionAnalyticsPath, "7", "exec-7"),
		fmt.Sprintf(evaluationRunPath, "7", "12"),
	} {
		recorder := serveRunAnalytics(router, path)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "Analytics is disabled") {
			t.Fatalf("%s: status = %d body = %s, want 403 Analytics is disabled", path, recorder.Code, recorder.Body.String())
		}
	}
	if len(repo.calls) != 0 {
		t.Fatalf("a request reached the repository with Analytics disabled: %v", repo.calls)
	}
}
