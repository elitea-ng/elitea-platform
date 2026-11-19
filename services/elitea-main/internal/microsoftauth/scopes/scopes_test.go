package scopes

import (
	"reflect"
	"strings"
	"testing"
)

// The cases follow the SDK's own behaviour (microsoft_graph_scopes.py,
// ms_graph.py normalize_ms_graph_scopes, oauth_app_mode.py sign_in_scopes).

func TestFixedListKeepsWhatItOffersInItsOrderAndCasing(t *testing.T) {
	cases := []struct {
		name    string
		list    List
		value   any
		want    []string
		dropped []string
	}{
		{"canonical order, not input order", Outlook, []string{"Mail.Send", "Mail.Read"}, []string{"Mail.Read", "Mail.Send"}, nil},
		{"case-insensitive", Outlook, []string{"mail.readwrite"}, []string{"Mail.ReadWrite"}, nil},
		{"graph prefix stripped", Outlook, []string{"https://graph.microsoft.com/Mail.Send"}, []string{"Mail.Send"}, nil},
		{"graph prefix in any case", Outlook, "HTTPS://GRAPH.MICROSOFT.COM/mail.read", []string{"Mail.Read"}, nil},
		{"comma and space separated string", Teams, "Chat.Read, ChatMessage.Send  Team.ReadBasic.All", []string{"Chat.Read", "ChatMessage.Send", "Team.ReadBasic.All"}, nil},
		{"unknown values are dropped and named", Outlook, []string{"Mail.Read", "Files.ReadWrite"}, []string{"Mail.Read"}, []string{"Files.ReadWrite"}},
		{"nothing left is the default", Outlook, []string{"Files.ReadWrite"}, []string{"Mail.Read"}, []string{"Files.ReadWrite"}},
		{"nil is the default", Teams, nil, Teams.Defaults(), nil},
		{"empty is the default", Outlook, []string{}, []string{"Mail.Read"}, nil},
		{"duplicates collapse", Outlook, []string{"Mail.Read", "mail.read"}, []string{"Mail.Read"}, nil},
		{"an []any from stored JSON", Teams, []any{"Chat.Read", "Chat.Create"}, []string{"Chat.Read", "Chat.Create"}, nil},
		{"offline_access is not offered", Outlook, []string{"offline_access", "Mail.Send"}, []string{"Mail.Send"}, []string{"offline_access"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := tc.list.Normalize(tc.value)
			if !reflect.DeepEqual(got, tc.want) || !reflect.DeepEqual(dropped, tc.dropped) {
				t.Fatalf("Normalize = %v dropped %v, want %v dropped %v", got, dropped, tc.want, tc.dropped)
			}
		})
	}
}

func TestTheListsAreTheSDKsAndTheDefaultsAreOffered(t *testing.T) {
	if got := strings.Join(Outlook.Names(), ","); got !=
		"Mail.Read,Mail.ReadWrite,Mail.Send,Mail.Read.Shared,Mail.ReadWrite.Shared,Mail.Send.Shared" {
		t.Errorf("Outlook = %s", got)
	}
	if got := strings.Join(Teams.Names(), ","); got !=
		"User.ReadBasic.All,Chat.Read,ChatMessage.Send,Chat.Create,Team.ReadBasic.All,Channel.ReadBasic.All,ChannelMessage.Send,ChannelMessage.Read.All" {
		t.Errorf("Teams = %s", got)
	}
	// ChannelMessage.Read.All needs Entra admin consent: off by default.
	for _, name := range Teams.Defaults() {
		if name == "ChannelMessage.Read.All" {
			t.Error("ChannelMessage.Read.All must be off by default")
		}
	}
	for _, list := range []List{Outlook, Teams} {
		offered := map[string]bool{}
		for _, name := range list.Names() {
			offered[name] = true
		}
		for _, name := range list.Defaults() {
			if !offered[name] {
				t.Errorf("default %q is not offered", name)
			}
		}
	}
}

func TestTheReturnedListsCannotChangeTheTable(t *testing.T) {
	Outlook.Defaults()[0] = "Mail.Send"
	Outlook.Options()[0].Name = "x"
	if Outlook.Defaults()[0] != "Mail.Read" || Outlook.Options()[0].Name != "Mail.Read" {
		t.Fatal("a caller changed the table")
	}
}

func TestMSGraphFreeText(t *testing.T) {
	good := []struct {
		name  string
		value any
		want  []string
	}{
		{"empty is User.Read", nil, []string{"User.Read"}},
		{"empty string", "  ", []string{"User.Read"}},
		{"bare names", []string{"Mail.Read", "Files.ReadWrite"}, []string{"Mail.Read", "Files.ReadWrite"}},
		{"prefix stripped", "https://graph.microsoft.com/Mail.Read", []string{"Mail.Read"}},
		{"OIDC scopes are handled by the platform", []string{"openid", "profile", "email", "offline_access", "Mail.Read"}, []string{"Mail.Read"}},
		{"only OIDC scopes is the default", "openid offline_access", []string{"User.Read"}},
		{"duplicates drop case-insensitively, first spelling wins", []string{"Mail.Read", "mail.read", "MAIL.READ"}, []string{"Mail.Read"}},
		{".default alone keeps the full scope", ".default", []string{GraphDefaultScope}},
		{"full .default alone", "https://graph.microsoft.com/.default", []string{GraphDefaultScope}},
		{"comma separated", "Mail.Read,Sites.Read.All", []string{"Mail.Read", "Sites.Read.All"}},
		{"a list item with spaces", []string{"Mail.Read Mail.Send"}, []string{"Mail.Read", "Mail.Send"}},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeMSGraph(tc.value)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("NormalizeMSGraph = %v, %v, want %v", got, err, tc.want)
			}
		})
	}
	bad := []struct {
		name  string
		value any
		want  string
	}{
		{"another resource", "https://outlook.office.com/Mail.Read", "not a Microsoft Graph permission"},
		{"a bare word", "mail", "not a Microsoft Graph permission"},
		{"a url that is not Graph", "https://example.test/.default", "not a Microsoft Graph permission"},
		{".default mixed", []string{".default", "Mail.Read"}, "cannot be combined"},
		{"full .default mixed", []string{"Mail.Read", "https://graph.microsoft.com/.default"}, "cannot be combined"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := NormalizeMSGraph(tc.value); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NormalizeMSGraph = %v, %v, want an error containing %q", got, err, tc.want)
			}
		})
	}
}

func TestSignInAsksForOfflineAccessUnlessRefreshIsOff(t *testing.T) {
	off, on := false, true
	cases := []struct {
		name string
		in   []string
		auto *bool
		want []string
	}{
		{"unset keeps it (older credentials never stored the switch)", []string{"Mail.Read"}, nil, []string{"offline_access", "Mail.Read"}},
		{"on", []string{"Mail.Read"}, &on, []string{"offline_access", "Mail.Read"}},
		{"off", []string{"Mail.Read"}, &off, []string{"Mail.Read"}},
		{"never twice", []string{"offline_access", "Mail.Read"}, nil, []string{"offline_access", "Mail.Read"}},
		{"off removes a stored one", []string{"offline_access", "Mail.Read"}, &off, []string{"Mail.Read"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SignIn(tc.in, tc.auto); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SignIn = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDataverseScopeIsTheOrgsUserImpersonation(t *testing.T) {
	if got := DataverseScope("https://org.crm4.dynamics.com/"); got != "https://org.crm4.dynamics.com/user_impersonation" {
		t.Fatalf("DataverseScope = %s", got)
	}
}

func TestMissingPermissionHintNamesThePermission(t *testing.T) {
	if hint := MissingPermissionHint("Mail.Send"); !strings.Contains(hint, "Mail.Send") || !strings.Contains(hint, "sign in again") {
		t.Errorf("hint = %q", hint)
	}
	if hint := MissingPermissionHint(""); !strings.Contains(hint, "tick it") {
		t.Errorf("generic hint = %q", hint)
	}
	if OutlookToolPermission("send_mail") != "Mail.Send" || OutlookToolPermission("delete_message") != "Mail.ReadWrite" ||
		TeamsToolPermission("search_teams_messages") != "ChannelMessage.Read.All" ||
		TeamsToolPermission("send_channel_message") != "ChannelMessage.Send" {
		t.Error("a tool names the wrong permission")
	}
}

// Every tool names a permission its own list offers, so the hint never tells a
// person to tick a box that does not exist.
func TestEveryToolNamesAPermissionItsListOffers(t *testing.T) {
	check := func(list List, tools map[string]string) {
		offered := map[string]bool{}
		for _, name := range list.Names() {
			offered[name] = true
		}
		for tool, permission := range tools {
			if !offered[permission] {
				t.Errorf("%s needs %s, which the list does not offer", tool, permission)
			}
		}
	}
	check(Outlook, outlookTools)
	check(Teams, teamsTools)
	if len(outlookTools) != 12 || len(teamsTools) != 7 {
		t.Errorf("tools = %d outlook, %d teams, want 12 and 7", len(outlookTools), len(teamsTools))
	}
}
