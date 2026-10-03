package eliteacore

// Load Tools against a real pre-built MCP catalogue table (shared migrations
// 0094 and 0128): the catalogue's credential headers reach only the
// catalogue's origin, never a URL the caller chose.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

func TestMCPSyncToolsSendsCatalogueHeadersOnlyToTheCatalogueOrigin(t *testing.T) {
	pool := newMCPCataloguePool(t)

	type seenRequest struct{ host, authorization, apiKey string }
	var mu sync.Mutex
	var seen []seenRequest
	mcpServer := func() *httptest.Server {
		return httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, seenRequest{r.Host, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")})
			mu.Unlock()
			var rpc struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&rpc)
			if rpc.Method == "notifications/initialized" {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": rpc.ID,
				"result": map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}})
		}))
	}
	catalogue := mcpServer()
	catalogue.StartTLS()
	defer catalogue.Close()
	attacker := mcpServer()
	attacker.TLS = catalogue.TLS
	attacker.StartTLS()
	defer attacker.Close()
	// One client trusts both httptest certificates (they share the test CA).
	client := catalogue.Client()

	store := mcpregistry.NewPrebuiltStore(pool)
	_, err := store.Upsert(context.Background(), mcpregistry.PrebuiltServer{
		Key: "shared_pat", DisplayName: "Shared PAT", Enabled: true,
		ServerURL: catalogue.URL + "/mcp",
		Headers:   map[string]string{"Authorization": "Bearer operator-shared-pat", "X-Api-Key": "operator-key"},
	})
	require.NoError(t, err)
	handler := NewHandler(nil, WithHTTPClient(client), WithPrebuiltMCPCatalogue(store, nil))

	call := func(body string) map[string]any {
		recorder := httptest.NewRecorder()
		handler.MCPSyncTools(recorder, syncRequest(body))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		var result map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result))
		return result
	}
	attackerHost := strings.TrimPrefix(attacker.URL, "https://")
	catalogueHost := strings.TrimPrefix(catalogue.URL, "https://")

	// A caller URL at another origin with blank headers: dialled, but bare.
	call(`{"toolkit_type":"mcp_shared_pat","url":"` + attacker.URL + `/mcp","headers":{}}`)
	// The catalogue URL (omitted, or the caller restating it) gets the headers.
	call(`{"toolkit_type":"mcp_shared_pat"}`)
	call(`{"toolkit_type":"mcp_shared_pat","url":"` + catalogue.URL + `/mcp"}`)

	mu.Lock()
	defer mu.Unlock()
	var attackerRequests, catalogueRequests int
	for _, request := range seen {
		switch request.host {
		case attackerHost:
			attackerRequests++
			require.Empty(t, request.authorization, "catalogue Authorization leaked to a caller-chosen origin")
			require.Empty(t, request.apiKey, "catalogue X-Api-Key leaked to a caller-chosen origin")
		case catalogueHost:
			catalogueRequests++
			require.Equal(t, "Bearer operator-shared-pat", request.authorization)
			require.Equal(t, "operator-key", request.apiKey)
		}
	}
	require.NotZero(t, attackerRequests)
	require.NotZero(t, catalogueRequests)
}

func newMCPCataloguePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)
	databaseName := fmt.Sprintf("elitea_mcp_sync_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+quoted)
	require.NoError(t, err)
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})
	for _, name := range []string{"0094_mcp_prebuilt_catalogue.sql", "0128_mcp_prebuilt_parameter_schema.sql"} {
		migration, err := os.ReadFile("../../../../migrations/shared/" + name)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(migration))
		require.NoError(t, err, "apply migration %s", name)
	}
	return pool
}
