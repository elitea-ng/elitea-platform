package microsoftapps

import (
	"reflect"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth/scopes"
)

// The Outlook and Teams lists and every type's defaults have ONE source,
// microsoftauth/scopes, which the credential save, the sign-in and the connection
// check read. This registry reads the same lists, so an administrator's ceiling can
// never offer a permission the credential form does not, or the reverse.
func TestOfferedListsAndDefaultsAreTheCredentialsOwn(t *testing.T) {
	if got, want := Offered(TypeOutlook), scopes.Outlook.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("outlook offered = %v, want %v", got, want)
	}
	if got, want := Offered(TypeTeams), scopes.Teams.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("teams offered = %v, want %v", got, want)
	}
	if got, want := Defaults(TypeOutlook), scopes.Outlook.Defaults(); !reflect.DeepEqual(got, want) {
		t.Errorf("outlook defaults = %v, want %v", got, want)
	}
	if got, want := Defaults(TypeTeams), scopes.Teams.Defaults(); !reflect.DeepEqual(got, want) {
		t.Errorf("teams defaults = %v, want %v", got, want)
	}
	if got, want := Defaults(TypeMSGraph), scopes.MSGraphDefault(); !reflect.DeepEqual(got, want) {
		t.Errorf("ms_graph defaults = %v, want %v", got, want)
	}
	if len(Offered(TypeMSGraph)) != 0 {
		t.Error("a free-text type offers no fixed list")
	}
	// A caller cannot change the shared list through the copy it was handed.
	got := Offered(TypeOutlook)
	got[0] = "Changed.Read"
	if Offered(TypeOutlook)[0] != scopes.Outlook.Names()[0] {
		t.Error("Offered shares its backing array")
	}
}

func TestNormalizeCeilingPerType(t *testing.T) {
	got, err := NormalizeCeiling(TypeTeams, []string{"channelmessage.send", "Chat.Read", "chat.read"})
	if err != nil || !reflect.DeepEqual(got, []string{"Chat.Read", "ChannelMessage.Send"}) {
		t.Errorf("teams = %v, %v", got, err)
	}
	if _, err := NormalizeCeiling(TypeOutlook, []string{"Files.Read"}); err == nil {
		t.Error("a permission outside the Outlook list was accepted")
	}
	got, err = NormalizeCeiling(TypeMSGraph, []string{"https://graph.microsoft.com/Files.Read", "files.read", "Sites.Read.All"})
	if err != nil || !reflect.DeepEqual(got, []string{"Files.Read", "Sites.Read.All"}) {
		t.Errorf("ms_graph = %v, %v", got, err)
	}
	for _, bad := range []string{".default", "https://graph.microsoft.com/.default", "openid", "https://other.example/Mail.Read", "Mail Read"} {
		if _, err := NormalizeCeiling(TypeMSGraph, []string{bad}); err == nil {
			t.Errorf("%q was accepted as a ceiling", bad)
		}
	}
	if _, err := NormalizeCeiling(TypeDataverse, []string{"Mail.Read"}); err == nil {
		t.Error("a Dataverse ceiling list was accepted")
	}
	if got, err := NormalizeCeiling(TypeDataverse, nil); err != nil || got != nil {
		t.Errorf("dataverse nil = %v, %v", got, err)
	}
}

func TestCeilingFallsBackToLeastPrivilege(t *testing.T) {
	app := App{ScopeCeiling: map[string][]string{TypeTeams: {"Chat.Read"}}}
	if got := app.Ceiling(TypeOutlook); !reflect.DeepEqual(got, []string{"Mail.Read"}) {
		t.Errorf("outlook default = %v", got)
	}
	if got := app.Ceiling(TypeTeams); !reflect.DeepEqual(got, []string{"Chat.Read"}) {
		t.Errorf("teams saved = %v", got)
	}
	if got := app.Ceiling(TypeMSGraph); !reflect.DeepEqual(got, []string{"User.Read"}) {
		t.Errorf("ms_graph default = %v", got)
	}
	// A returned list is a copy: editing it must not edit the app.
	app.Ceiling(TypeTeams)[0] = "Chat.ReadWrite"
	if app.ScopeCeiling[TypeTeams][0] != "Chat.Read" {
		t.Error("Ceiling leaked its backing array")
	}
}

func TestIntersectCutsToTheCeilingAndReportsWhatItCut(t *testing.T) {
	effective, removed := Intersect([]string{"Mail.Read", "Mail.Send"}, []string{"mail.send", "Mail.ReadWrite", "https://graph.microsoft.com/Mail.Read"})
	if !reflect.DeepEqual(effective, []string{"Mail.Read", "Mail.Send"}) || !reflect.DeepEqual(removed, []string{"Mail.ReadWrite"}) {
		t.Errorf("effective %v removed %v", effective, removed)
	}
	if effective, _ := Intersect([]string{"Mail.Read"}, []string{"Mail.Send"}); len(effective) != 0 {
		t.Errorf("a request wholly above the ceiling must grant nothing, got %v", effective)
	}
}

func TestRequiredPermissionsFollowTheCeilingsAndTheRefreshSwitch(t *testing.T) {
	app := App{
		CredentialTypes: []string{TypeTeams, TypeDataverse}, AutoRefreshToken: true,
		ScopeCeiling: map[string][]string{TypeTeams: {"ChannelMessage.Read.All", "Chat.Read"}},
	}
	resources := app.RequiredPermissions()
	if len(resources) != 2 || resources[0].API != "Microsoft Graph" || resources[1].API != "Dynamics CRM" {
		t.Fatalf("resources = %+v", resources)
	}
	var names []string
	consent := map[string]bool{}
	for _, p := range resources[0].Permissions {
		names = append(names, p.Name)
		consent[p.Name] = p.AdminConsent
	}
	if !reflect.DeepEqual(names, []string{"openid", "profile", "offline_access", "ChannelMessage.Read.All", "Chat.Read"}) {
		t.Errorf("graph permissions = %v", names)
	}
	if !consent["ChannelMessage.Read.All"] || consent["Chat.Read"] {
		t.Errorf("admin consent flags = %v", consent)
	}
	if got := resources[1].Permissions; len(got) != 1 || got[0].Name != "user_impersonation" {
		t.Errorf("dataverse permissions = %v", got)
	}
	app.AutoRefreshToken = false
	for _, p := range app.RequiredPermissions()[0].Permissions {
		if p.Name == "offline_access" {
			t.Error("offline_access listed although token refresh is off")
		}
	}
}

func TestEffectiveScopesAppliesTheTypeDefaultsThenTheCeiling(t *testing.T) {
	app := App{ScopeCeiling: map[string][]string{TypeTeams: {"Chat.Read"}}}
	// Nothing ticked: the type's defaults, cut to the ceiling.
	effective, removed := app.EffectiveScopes(TypeTeams, nil)
	if !reflect.DeepEqual(effective, []string{"Chat.Read"}) || !reflect.DeepEqual(removed, []string{"User.ReadBasic.All", "Team.ReadBasic.All", "Channel.ReadBasic.All"}) {
		t.Errorf("teams defaults = %v removed %v", effective, removed)
	}
	// Outlook has no saved ceiling, so its default ceiling admits its default request.
	if effective, removed := app.EffectiveScopes(TypeOutlook, nil); !reflect.DeepEqual(effective, []string{"Mail.Read"}) || len(removed) != 0 {
		t.Errorf("outlook defaults = %v removed %v", effective, removed)
	}
	// Something above the ceiling is cut and reported.
	effective, removed = app.EffectiveScopes(TypeOutlook, []string{"Mail.Send"})
	if len(effective) != 0 || !reflect.DeepEqual(removed, []string{"Mail.Send"}) {
		t.Errorf("above the ceiling = %v removed %v", effective, removed)
	}
	// Intersect itself does not guess a default.
	if effective, _ := Intersect([]string{"Mail.Read"}, nil); len(effective) != 0 {
		t.Errorf("Intersect of nothing = %v", effective)
	}
}
