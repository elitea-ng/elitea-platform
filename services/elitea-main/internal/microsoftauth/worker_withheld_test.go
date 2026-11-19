package microsoftauth

import (
	"reflect"
	"testing"
)

// The worker holds an access token only (ADR-0057 decision 7): the four
// delegated-only types lose the client secret and the stored refresh token,
// and SharePoint, whose worker config still reads them, is left alone.
func TestWithholdFromWorkerStripsTheMintingFieldsOfTheFourTypes(t *testing.T) {
	for _, configType := range []string{Outlook, Teams, MSGraph, Dataverse} {
		data := map[string]any{
			"client_id": "client", "client_secret": "secret", "refresh_token": "refresh",
			"oauth_discovery_endpoint": "https://login.microsoftonline.com/tenant", "scopes": []any{"Mail.Read"},
		}
		if !WithholdFromWorker(configType, data) {
			t.Fatalf("%s: nothing withheld", configType)
		}
		want := map[string]any{
			"client_id":                "client",
			"oauth_discovery_endpoint": "https://login.microsoftonline.com/tenant", "scopes": []any{"Mail.Read"},
		}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("%s: data = %#v", configType, data)
		}
		if WithholdFromWorker(configType, data) {
			t.Fatalf("%s: a second pass found more to withhold", configType)
		}
	}
}

func TestWithholdFromWorkerLeavesSharePointAndOtherTypesAlone(t *testing.T) {
	for _, configType := range []string{SharePoint, "jira", ""} {
		data := map[string]any{"client_secret": "secret", "refresh_token": "refresh"}
		if WithholdFromWorker(configType, data) || len(data) != 2 {
			t.Fatalf("%q: data = %#v", configType, data)
		}
	}
}
