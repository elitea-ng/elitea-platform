package configurations

import (
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth/scopes"
)

// The four Microsoft delegated credential types (ADR-0057, epic #2254 story
// S1a): Outlook, Teams, Microsoft Graph and Dataverse. They are defined natively
// (D1, docs/contributing/adding-types.md) from the SDK's
// elitea_sdk/configurations/{outlook,teams,ms_graph,dataverse}.py, with the
// deltas the ADR decides, which the fixtures under testdata/sdk-fixtures record:
//
//   - no `refresh_token` field. A refresh token is never written into a
//     credential's data (decision 4): a credential PUT cannot carry it or clear
//     it by accident, and no export of a credential can contain it.
//   - Dataverse gains `oauth_app_id` (Default app mode, decision 2), and its
//     `client_id` and `client_secret` become optional like the other three,
//     because a Default app holds the client.
//
// Their connection check is the delegated one of
// api/v2/configurations/check_connection_oauth.go, driven by the descriptors in
// internal/microsoftauth: the 401 `requires_authorization` answer, or an
// identity read when a token comes with the check. None needs a probe in
// toolkit_check.go.

const entraAuthorityDescription = "OAuth Discovery Endpoint. Format: https://login.microsoftonline.com/{tenant_id}"

func nullable(schema map[string]any) map[string]any {
	return map[string]any{"anyOf": []any{schema, map[string]any{"type": "null"}}}
}

func withMeta(field map[string]any, meta map[string]any) map[string]any {
	for key, value := range meta {
		field[key] = value
	}
	return field
}

func optionalString(title, description string) map[string]any {
	return withMeta(nullable(map[string]any{"type": "string"}), map[string]any{
		"default": nil, "title": title, "description": description,
	})
}

func optionalSecret(title, description string) map[string]any {
	return withMeta(nullable(map[string]any{"type": "string", "format": "password", "writeOnly": true}), map[string]any{
		"default": nil, "title": title, "description": description,
	})
}

func scopesField(options []scopes.Option, defaults []string, note string) map[string]any {
	field := withMeta(nullable(map[string]any{"type": "array", "items": map[string]any{"type": "string"}}),
		map[string]any{"default": stringsAsAny(defaults), "title": "Scopes", "description": note})
	if len(options) > 0 {
		checkboxes := make([]any, 0, len(options))
		for _, option := range options {
			checkboxes = append(checkboxes, map[string]any{
				"value": option.Name, "label": option.Name, "description": option.Description,
			})
		}
		field["ui_component"] = "checkbox_list"
		field["checkbox_options"] = checkboxes
	}
	return field
}

func stringsAsAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

// graphScopesNote is the SDK's SCOPES_NOTE (Outlook and Teams).
const graphScopesNote = "Microsoft Graph permissions requested at sign-in. ELITEA passes them to Microsoft as-is.\n\n" +
	"- The token always holds every permission you already approved for this Entra app, " +
	"so unticking a box does not remove access granted before.\n" +
	"- To remove a granted permission, revoke it on the app's Manage your application page " +
	"at https://myapps.microsoft.com (or ask your Entra admin), then sign in again.\n" +
	"- To strictly limit what a toolkit can do, use a separate Entra app that has only " +
	"the permissions you need.\n" +
	"- offline_access is always added so the token can be refreshed."

// msGraphScopesNote is the SDK's MS_GRAPH_SCOPES_NOTE.
const msGraphScopesNote = "Microsoft Graph delegated permissions requested at sign-in, e.g. Mail.Read, Files.ReadWrite, Sites.Read.All. " +
	"ELITEA passes them to Microsoft as-is; empty means User.Read.\n\n" +
	"- Only Microsoft Graph permissions: a bare name or https://graph.microsoft.com/<name>. " +
	"https://graph.microsoft.com/.default requests every permission the app was granted.\n" +
	"- The token always holds every permission you already approved for this Entra app.\n" +
	"- After adding a permission, sign in again so the token gets it.\n" +
	"- offline_access is always added so the token can be refreshed."

func oauthAppField() map[string]any {
	return withMeta(nullable(map[string]any{"type": "string"}), map[string]any{
		"default": nil, "title": "App", "description": "OAuth app configured by your administrator",
		"ui_component": "oauth_app_select",
	})
}

func autoRefreshField() map[string]any {
	return withMeta(nullable(map[string]any{"type": "boolean"}), map[string]any{
		"default": true, "title": "Auto Refresh Token",
		"description": "Automatically refresh the access token using the offline_access scope.",
	})
}

// microsoftGraphProperties are the fields Outlook, Teams and Microsoft Graph
// share: Default app (oauth_app_id) or Custom app (the rest).
func microsoftGraphProperties(scopeField map[string]any) map[string]any {
	return map[string]any{
		"oauth_app_id":             oauthAppField(),
		"client_id":                optionalString("Client Id", "Azure AD Application (client) ID"),
		"client_secret":            optionalSecret("Client Secret", "Azure AD Client Secret"),
		"oauth_discovery_endpoint": optionalString("Oauth Discovery Endpoint", entraAuthorityDescription),
		"scopes":                   scopeField,
		"auto_refresh_token":       autoRefreshField(),
	}
}

func microsoftAuthSubsections() []NativeAuthSubsection {
	return []NativeAuthSubsection{
		{Name: "Default app", Fields: []string{microsoftauth.FieldAppID}},
		{Name: "Custom app", Fields: []string{
			"client_id", "client_secret", microsoftauth.FieldDiscoveryEndpoint, microsoftauth.FieldScopes,
			microsoftauth.FieldAutoRefresh,
		}},
	}
}

func microsoftNativeTypes() []NativeConfigurationType {
	outlook := microsoftGraphProperties(scopesField(scopes.Outlook.Options(), scopes.Outlook.Defaults(), graphScopesNote))
	outlook["mailbox"] = optionalString("Mailbox",
		"Leave empty to use your own mailbox. Set only to open a shared mailbox you have Full Access to "+
			"(needs the Mail.Read.Shared scope, plus Mail.ReadWrite.Shared / Mail.Send.Shared to change or send). "+
			"A distribution list is not a mailbox: its mail is already in your inbox, so leave this empty")
	teams := microsoftGraphProperties(scopesField(scopes.Teams.Options(), scopes.Teams.Defaults(), graphScopesNote))
	msGraph := microsoftGraphProperties(scopesField(nil, scopes.MSGraphDefault(), msGraphScopesNote))

	outlookSubsections := microsoftAuthSubsections()
	outlookSubsections[1].Fields = append(outlookSubsections[1].Fields, microsoftauth.FieldMailbox)

	return []NativeConfigurationType{
		{
			Type: microsoftauth.Outlook, Section: "credentials", Title: "OutlookConfiguration", Label: "Outlook",
			Categories: []string{"office"}, ExtraCategories: []string{"outlook", "microsoft", "email", "mail"},
			Properties: outlook, AuthSubsections: outlookSubsections, CheckConnection: true,
		},
		{
			Type: microsoftauth.Teams, Section: "credentials", Title: "TeamsConfiguration", Label: "Teams",
			Categories: []string{"office"}, ExtraCategories: []string{"teams", "microsoft", "chat", "messaging"},
			Properties: teams, AuthSubsections: microsoftAuthSubsections(), CheckConnection: true,
		},
		{
			Type: microsoftauth.MSGraph, Section: "credentials", Title: "MsGraphConfiguration", Label: "Microsoft Graph",
			Categories: []string{"office"}, ExtraCategories: []string{"microsoft", "graph", "office 365", "microsoft 365", "entra"},
			Properties: msGraph, AuthSubsections: microsoftAuthSubsections(), CheckConnection: true,
		},
		{
			Type: microsoftauth.Dataverse, Section: "credentials", Title: "DataverseConfiguration", Label: "Dataverse (OneCRM)",
			Categories: []string{"other"}, ExtraCategories: []string{"dataverse", "dynamics", "crm", "onecrm", "microsoft"},
			Properties: map[string]any{
				"url": map[string]any{
					"type": "string", "title": "Url",
					"description": "Dataverse environment URL, e.g. https://onecrmepam.crm4.dynamics.com",
				},
				"tenant_id": map[string]any{
					"type": "string", "title": "Tenant Id", "description": "Entra tenant ID (GUID) or verified domain",
				},
				"oauth_app_id":  oauthAppField(),
				"client_id":     optionalString("Client Id", "Entra app (client) ID"),
				"client_secret": optionalSecret("Client Secret", "Entra app client secret"),
			},
			Required: []string{"url", "tenant_id"},
			AuthSubsections: []NativeAuthSubsection{
				{Name: "Default app", Fields: []string{microsoftauth.FieldAppID}},
				{Name: "Custom app", Fields: []string{"client_id", "client_secret"}},
			},
			CheckConnection: true,
		},
	}
}
