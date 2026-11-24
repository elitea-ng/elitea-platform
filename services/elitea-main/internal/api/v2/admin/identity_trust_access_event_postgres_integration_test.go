package admin

// The identity_trust kind of admin.access.changed (ADR-0059 R1.5, issue
// 2509): an identity provider create, trust change or delete and a SCIM
// client create or rotate append one chained event in the store's
// transaction, naming the fields that changed and never a value. A failed
// append rolls the write back. The role kinds are in
// access_changed_event_postgres_integration_test.go.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/scimclient"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

type trustEvent struct {
	Subject   string
	Action    string
	Principal string
	Fields    []string
	Before    []string
	After     []string
}

func TestIdentityTrustChangesAppendOneAccessChangedEventEach(t *testing.T) {
	pool := newRebindCorpusPool(t)
	ctx := context.Background()
	handler := NewHandler(pool,
		WithIdentityProviders(identityproviders.NewStore(pool), newRecordingVault()),
		WithSCIMClients(scimclient.NewStore(pool, time.Hour)),
		superAdmin)

	read := func() []trustEvent {
		rows, err := pool.Query(ctx, `
SELECT event FROM elitea_runtime.security_audit_outbox WHERE event_type = $1 ORDER BY seq`,
			string(securityaudit.TypeAdminAccessChanged))
		require.NoError(t, err)
		defer rows.Close()
		var events []trustEvent
		for rows.Next() {
			var raw []byte
			require.NoError(t, rows.Scan(&raw))
			var envelope struct {
				Subject string `json:"subject"`
				Data    struct {
					Action       string `json:"action"`
					PrincipalRef string `json:"principal_ref"`
					Access       struct {
						Fields []string `json:"fields"`
						Before []string `json:"before"`
						After  []string `json:"after"`
					} `json:"access"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(raw, &envelope))
			require.NotContains(t, string(raw), "client-secret-value", "an event carried a secret value")
			events = append(events, trustEvent{
				Subject: envelope.Subject, Action: envelope.Data.Action, Principal: envelope.Data.PrincipalRef,
				Fields: envelope.Data.Access.Fields, Before: envelope.Data.Access.Before, After: envelope.Data.Access.After,
			})
		}
		require.NoError(t, rows.Err())
		return events
	}

	// A new provider (with a secret) is a trust change.
	body := strings.Replace(validOIDCBody, "{", `{"secret": "client-secret-value",`, 1)
	recorder, _ := audited(providerRequest(http.MethodPut, "corporate", body), handler.IdentityProviderSave)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	events := read()
	require.Len(t, events, 1)
	require.Equal(t, "admin.access.identity_trust", events[0].Action)
	require.Equal(t, "identity_provider/corporate", events[0].Subject)
	require.Equal(t, "user:5", events[0].Principal)
	require.Contains(t, events[0].Fields, "created")
	require.Empty(t, events[0].Before)
	require.Empty(t, events[0].After)

	// A settings-only save (the display name) is not a trust change.
	settings := strings.Replace(validOIDCBody, `"display_name": "Corporate SSO"`, `"display_name": "Corporate"`, 1)
	require.NotEqual(t, validOIDCBody, settings, "the fixture body has no display name to change")
	recorder, _ = audited(providerRequest(http.MethodPut, "corporate", settings), handler.IdentityProviderSave)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Len(t, read(), 1, "a settings-only save appended an event")

	// The delete is the second trust event.
	recorder, _ = audited(providerRequest(http.MethodDelete, "corporate", ""), handler.IdentityProviderDelete)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	events = read()
	require.Len(t, events, 2)
	require.Equal(t, []string{"deleted"}, events[1].Fields)

	// A SCIM client create and rotate: one event each.
	router := scimClientRouter(handler)
	created := callSCIMClients(router, http.MethodPost, "/scim_clients/administration",
		`{"name":"entra","auth_method":"bearer"}`)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var issued struct {
		Client struct {
			ID string `json:"id"`
		} `json:"client"`
		Secret string `json:"secret"`
	}
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &issued))
	rotated := callSCIMClients(router, http.MethodPost,
		"/scim_clients/administration/"+issued.Client.ID+"/rotate", "")
	require.Equal(t, http.StatusOK, rotated.Code, rotated.Body.String())
	events = read()
	require.Len(t, events, 4)
	require.Equal(t, "scim_client/"+issued.Client.ID, events[2].Subject)
	require.Equal(t, []string{"created", "secret"}, events[2].Fields)
	require.Equal(t, []string{"secret"}, events[3].Fields)
	require.Equal(t, "user:42", events[3].Principal)
	for _, event := range events {
		require.NotEmpty(t, issued.Secret)
		require.NotContains(t, strings.Join(event.Fields, ","), issued.Secret)
	}

	report, err := securityaudit.Verify(ctx, pool, securityaudit.SourceOutbox, 0, 1)
	require.NoError(t, err)
	require.Nil(t, report.Break)
	require.EqualValues(t, 4, report.Checked)
}

// A failed append rolls the identity provider write back.
func TestAFailedAppendRollsTheIdentityTrustChangeBack(t *testing.T) {
	pool := newRebindCorpusPool(t)
	handler := NewHandler(pool,
		WithIdentityProviders(identityproviders.NewStore(pool), newRecordingVault()),
		WithSCIMClients(scimclient.NewStore(pool, time.Hour)),
		WithAccessAuditAppender(securityaudit.FailingAppender{Err: errors.New("injected append failure")}),
		superAdmin)

	recorder := httptest.NewRecorder()
	handler.IdentityProviderSave(recorder, providerRequest(http.MethodPut, "corporate", validOIDCBody))
	require.NotEqual(t, http.StatusOK, recorder.Code, recorder.Body.String())
	_, err := identityproviders.NewStore(pool).Lookup(context.Background(), "corporate")
	require.ErrorIs(t, err, identityproviders.ErrNotFound, "the provider was written without its event")

	created := callSCIMClients(scimClientRouter(handler), http.MethodPost, "/scim_clients/administration",
		`{"name":"entra","auth_method":"bearer"}`)
	require.NotEqual(t, http.StatusCreated, created.Code, created.Body.String())
	var clients int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM elitea_auth.scim_clients`).Scan(&clients))
	require.Zero(t, clients, "the SCIM client was written without its event")
}
