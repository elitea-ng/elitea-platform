package eliteacore_test

// #1286: a client secret interpolated from a GLOBAL secret, or taken from a
// configuration another project owns, must not be released to a token endpoint
// the toolkit's editor wrote. Only the owner's own endpoint fields bind it.
//
// Provenance is the difference between the claim-mode settings (Settings) and
// the same row resolved without the admin-vault fallback (ProjectOwnedSettings).
// The stub below hands the proxy both, as the composed resolver does.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	toolkitexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
)

const (
	provenanceAttackerEndpoint = "https://collector.example/token"
	provenanceOwnerEndpoint    = "https://login.operator.example/oauth2/token"
	provenanceGlobalPlaintext  = "operator-global-secret"
)

type provenanceCase struct {
	name              string
	toolkitType       string
	settings          map[string]any
	projectOwned      map[string]any
	withoutProvenance bool
	tokenEndpoint     string
	wantReleased      bool
}

func runProvenanceCase(t *testing.T, tc provenanceCase) {
	t.Helper()
	resolver := &delegatedAuthSettingsStub{
		found: true,
		result: toolkitexecutionapp.DelegatedAuthToolkitSettings{
			ToolkitType:          tc.toolkitType,
			Settings:             tc.settings,
			ProjectOwnedSettings: tc.projectOwned,
		},
		withoutProvenance: tc.withoutProvenance,
	}
	var released []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		released = append(released, request.URL.String()+" "+request.Form.Get("client_secret"))
		return oauthResponse(request, http.StatusOK, `{"access_token":"access"}`), nil
	})}
	handler := eliteacore.NewHandler(
		nil,
		eliteacore.WithHTTPClient(client),
		eliteacore.WithDelegatedAuthToolkitSettingsResolver(resolver),
	)
	request := authenticatedOAuthRequest(t, `{
		"token_endpoint":"`+tc.tokenEndpoint+`",
		"code":"code",
		"redirect_uri":"https://elitea.example/mcp-auth-callback",
		"client_secret":"********",
		"toolkit_id":19
	}`)
	recorder := httptest.NewRecorder()
	handler.MCPOAuthProxy(recorder, request)

	if !tc.wantReleased {
		assertStatus(t, recorder, http.StatusBadRequest)
		if len(released) != 0 {
			t.Fatalf("the secret reached the transport: %v", released)
		}
		if response := decodeObj(t, recorder); response["error"] != "untrusted_token_endpoint" {
			t.Fatalf("unexpected refusal: %#v", response)
		}
		return
	}
	assertStatus(t, recorder, http.StatusOK)
	if len(released) != 1 {
		t.Fatalf("token requests = %v, want exactly one", released)
	}
}

func TestOAuthProxyRefusesAGlobalSecretAtAnEditorChosenEndpoint(t *testing.T) {
	for _, tc := range []provenanceCase{
		{
			// The issue's attack: {{secret.<global>}} in the toolkit's own
			// settings, token endpoint pointed at the editor's host.
			name:        "global vault secret in the toolkit's own settings",
			toolkitType: "openapi",
			settings: map[string]any{
				"client_id": "app", "client_secret": provenanceGlobalPlaintext,
				"token_url": provenanceAttackerEndpoint,
			},
			projectOwned: map[string]any{
				"client_id": "app", "client_secret": "{{secret.platform_oauth}}",
				"token_url": provenanceAttackerEndpoint,
			},
			tokenEndpoint: provenanceAttackerEndpoint,
		},
		{
			name:        "global reference embedded in a longer value",
			toolkitType: "openapi",
			settings: map[string]any{
				"client_id": "app", "client_secret": "x-" + provenanceGlobalPlaintext,
				"oauth_token_endpoint": provenanceAttackerEndpoint,
			},
			projectOwned: map[string]any{
				"client_id": "app", "client_secret": "x-{{secret.platform_oauth}}",
				"oauth_token_endpoint": provenanceAttackerEndpoint,
			},
			tokenEndpoint: provenanceAttackerEndpoint,
		},
		{
			// A project-owned configuration is editable by the project, so a
			// global reference inside it is no better than one in the toolkit.
			name:        "global vault secret inside the project's own configuration",
			toolkitType: "openapi",
			settings: map[string]any{"openapi_configuration": map[string]any{
				"client_id": "app", "client_secret": provenanceGlobalPlaintext,
				"token_endpoint":           provenanceAttackerEndpoint,
				"configuration_project_id": int32(7),
			}},
			projectOwned: map[string]any{"openapi_configuration": map[string]any{
				"client_id": "app", "client_secret": "{{secret.platform_oauth}}",
				"token_endpoint":           provenanceAttackerEndpoint,
				"configuration_project_id": int32(7),
			}},
			tokenEndpoint: provenanceAttackerEndpoint,
		},
		{
			// A shared public-project configuration: its secret is redeemed
			// from the public project's vault in BOTH resolutions, so only the
			// owner stamp tells it apart. The endpoint is the editor's.
			name:        "shared configuration secret, endpoint in the toolkit's settings",
			toolkitType: "sharepoint",
			settings: map[string]any{
				"token_url": provenanceAttackerEndpoint,
				"sharepoint_configuration": map[string]any{
					"client_id": "app", "client_secret": provenanceGlobalPlaintext,
					"configuration_project_id": int32(1),
				},
			},
			tokenEndpoint: provenanceAttackerEndpoint,
		},
		{
			// private:true resolves the CALLER's personal configuration: the
			// editor must not be able to send the caller's secret anywhere.
			name:        "personal configuration secret, discovery endpoint in the toolkit's settings",
			toolkitType: "openapi",
			settings: map[string]any{
				"oauth_discovery_endpoint": "https://collector.example",
				"openapi_configuration": map[string]any{
					"client_id": "app", "client_secret": "callers-personal-secret",
					"configuration_project_id": float64(99),
				},
			},
			tokenEndpoint: provenanceAttackerEndpoint,
		},
		{
			// The owner stamp on the TOOLKIT layer is editor-written and is
			// never read: the secret came from the toolkit's own settings.
			name:        "forged owner stamp on the toolkit's own settings",
			toolkitType: "openapi",
			settings: map[string]any{
				"client_id": "app", "client_secret": provenanceGlobalPlaintext,
				"configuration_project_id": int32(1), "token_url": provenanceAttackerEndpoint,
			},
			projectOwned: map[string]any{
				"client_id": "app", "client_secret": "{{secret.platform_oauth}}",
				"configuration_project_id": int32(1), "token_url": provenanceAttackerEndpoint,
			},
			tokenEndpoint: provenanceAttackerEndpoint,
		},
		{
			// The secret comes from the toolkit layer; a shared configuration
			// that pins the attacker's endpoint lends that endpoint no trust
			// for a secret it did not supply.
			name:        "global toolkit secret beside a shared configuration's endpoint",
			toolkitType: "openapi",
			settings: map[string]any{
				"client_id": "app", "client_secret": provenanceGlobalPlaintext,
				"openapi_configuration": map[string]any{
					"token_url":                provenanceAttackerEndpoint,
					"configuration_project_id": int32(1),
				},
			},
			projectOwned: map[string]any{
				"client_id": "app", "client_secret": "{{secret.platform_oauth}}",
				"openapi_configuration": map[string]any{
					"token_url":                provenanceAttackerEndpoint,
					"configuration_project_id": int32(1),
				},
			},
			tokenEndpoint: provenanceAttackerEndpoint,
		},
		{
			// A direct MCP toolkit would otherwise fall through to discovery
			// from its saved url, which the editor also writes.
			name:        "global secret on a direct MCP toolkit never reaches discovery",
			toolkitType: "mcp",
			settings: map[string]any{
				"url":       "https://collector.example/mcp",
				"client_id": "app", "client_secret": provenanceGlobalPlaintext,
			},
			projectOwned: map[string]any{
				"url":       "https://collector.example/mcp",
				"client_id": "app", "client_secret": "{{secret.platform_oauth}}",
			},
			tokenEndpoint: provenanceAttackerEndpoint,
		},
		{
			// Fail closed: a resolver composed without the provenance twin
			// cannot vouch for any secret.
			name:        "no provenance available",
			toolkitType: "openapi",
			settings: map[string]any{
				"client_id": "app", "client_secret": "literal",
				"token_url": provenanceAttackerEndpoint,
			},
			withoutProvenance: true,
			tokenEndpoint:     provenanceAttackerEndpoint,
		},
	} {
		t.Run(tc.name, func(t *testing.T) { runProvenanceCase(t, tc) })
	}
}

func TestOAuthProxyStillBindsProjectOwnedSecretsAndOwnerPinnedEndpoints(t *testing.T) {
	for _, tc := range []provenanceCase{
		{
			// Literal and project-vault secrets: both resolutions agree, so the
			// toolkit's saved token settings keep binding (pre-#1286 behaviour).
			name:        "literal secret, endpoint in the toolkit's settings",
			toolkitType: "openapi",
			settings: map[string]any{
				"client_id": "app", "client_secret": "literal-secret",
				"token_url": provenanceAttackerEndpoint,
			},
			tokenEndpoint: provenanceAttackerEndpoint,
			wantReleased:  true,
		},
		{
			name:        "project configuration secret, endpoint in that configuration",
			toolkitType: "openapi",
			settings: map[string]any{"openapi_configuration": map[string]any{
				"client_id": "app", "client_secret": "project-vault-secret",
				"token_endpoint":           provenanceOwnerEndpoint,
				"configuration_project_id": int32(7),
			}},
			tokenEndpoint: provenanceOwnerEndpoint,
			wantReleased:  true,
		},
		{
			// The operator wrote both the secret and the endpoint of a shared
			// configuration, so its own endpoint binds its own secret.
			name:        "shared configuration secret, endpoint pinned in that configuration",
			toolkitType: "sharepoint",
			settings: map[string]any{
				"token_url": provenanceAttackerEndpoint,
				"sharepoint_configuration": map[string]any{
					"client_id": "app", "client_secret": provenanceGlobalPlaintext,
					"token_url":                provenanceOwnerEndpoint,
					"configuration_project_id": int32(1),
				},
			},
			tokenEndpoint: provenanceOwnerEndpoint,
			wantReleased:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) { runProvenanceCase(t, tc) })
	}
}
