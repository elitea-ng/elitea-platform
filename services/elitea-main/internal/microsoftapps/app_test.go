package microsoftapps

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testClientID = "7f8d0c1e-0000-4000-8000-000000000001"

func validInput() Input {
	return Input{
		Name: "Company Microsoft", ClientID: testClientID,
		Authority:       "https://login.microsoftonline.com/11111111-2222-4333-8444-555555555555",
		CredentialTypes: []string{TypeTeams, TypeOutlook}, AutoRefreshToken: true,
	}
}

func TestParseAuthorityAcceptsOnlyEntraTenants(t *testing.T) {
	accepted := map[string]string{
		"https://login.microsoftonline.com/organizations":                                 "https://login.microsoftonline.com/organizations",
		"https://login.microsoftonline.com/common/":                                       "https://login.microsoftonline.com/common",
		"HTTPS://LOGIN.MICROSOFTONLINE.COM/Contoso.Onmicrosoft.com/v2.0":                  "https://login.microsoftonline.com/contoso.onmicrosoft.com",
		"https://login.microsoftonline.com:443/11111111-2222-4333-8444-555555555555":      "https://login.microsoftonline.com/11111111-2222-4333-8444-555555555555",
		" https://login.microsoftonline.com/11111111-2222-4333-8444-555555555555/v2.0 \n": "https://login.microsoftonline.com/11111111-2222-4333-8444-555555555555",
	}
	for raw, want := range accepted {
		got, err := ParseAuthority(raw)
		if err != nil || got.URL != want {
			t.Errorf("ParseAuthority(%q) = %q, %v; want %q", raw, got.URL, err, want)
		}
	}
	refused := []string{
		"", "login.microsoftonline.com/common",
		"http://login.microsoftonline.com/common",
		"https://login.microsoftonline.us/common",
		"https://login.partner.microsoftonline.cn/common",
		"https://login.microsoftonline.com.evil.example/common",
		"https://evil.example/login.microsoftonline.com/common",
		"https://user@login.microsoftonline.com/common",
		"https://login.microsoftonline.com:8443/common",
		"https://login.microsoftonline.com/common?x=1",
		"https://login.microsoftonline.com/common#f",
		"https://login.microsoftonline.com/",
		"https://login.microsoftonline.com/common/oauth2/v2.0",
		"https://login.microsoftonline.com/consumers",
		"https://login.microsoftonline.com/not_a_tenant",
	}
	for _, raw := range refused {
		var validation ValidationError
		if _, err := ParseAuthority(raw); !errors.As(err, &validation) || validation.Field != "authority" {
			t.Errorf("ParseAuthority(%q) err = %v; want an authority validation error", raw, err)
		}
	}
}

func TestNormalizeIsCanonicalAndStrict(t *testing.T) {
	in := validInput()
	in.ClientID = " " + strings.ToUpper(testClientID) + " "
	in.Authority = "https://login.microsoftonline.com/ORGANIZATIONS/"
	in.CredentialTypes = []string{TypeTeams, TypeOutlook, TypeTeams}
	in.ScopeCeiling = map[string][]string{TypeOutlook: {"mail.send", "https://graph.microsoft.com/Mail.Read"}}
	out, err := in.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if out.ClientID != testClientID || out.Authority != "https://login.microsoftonline.com/organizations" {
		t.Errorf("not canonical: %q %q", out.ClientID, out.Authority)
	}
	if !reflect.DeepEqual(out.CredentialTypes, []string{TypeOutlook, TypeTeams}) {
		t.Errorf("types = %v", out.CredentialTypes)
	}
	if !reflect.DeepEqual(out.ScopeCeiling[TypeOutlook], []string{"Mail.Read", "Mail.Send"}) {
		t.Errorf("ceiling = %v", out.ScopeCeiling)
	}
	if out.ProjectScope != ScopeAll {
		t.Errorf("project scope = %q", out.ProjectScope)
	}

	bad := map[string]func(*Input){
		"name":             func(i *Input) { i.Name = "  " },
		"client_id":        func(i *Input) { i.ClientID = "not-a-guid" },
		"credential_types": func(i *Input) { i.CredentialTypes = []string{"gmail"} },
		"scope_ceiling":    func(i *Input) { i.ScopeCeiling = map[string][]string{TypeMSGraph: {"Mail.Read"}} },
		"project_ids":      func(i *Input) { i.ProjectScope = ScopeSelected },
		"secret":           func(i *Input) { s := "a\nb"; i.Secret = &s },
		"description":      func(i *Input) { i.Description = strings.Repeat("x", MaxDescriptionLength+1) },
	}
	for field, mutate := range bad {
		in := validInput()
		mutate(&in)
		var validation ValidationError
		if _, err := in.Normalize(); !errors.As(err, &validation) || validation.Field != field {
			t.Errorf("%s: err = %v", field, err)
		}
	}
	empty := validInput()
	empty.CredentialTypes = nil
	if _, err := empty.Normalize(); err == nil {
		t.Error("an app that serves no type was accepted")
	}
	listed := validInput()
	listed.ProjectIDs = []int64{4}
	if _, err := listed.Normalize(); err == nil {
		t.Error("projects with scope all were accepted")
	}
}

func TestSelectedProjectsAreSortedAndDeduplicated(t *testing.T) {
	in := validInput()
	in.ProjectScope, in.ProjectIDs = ScopeSelected, []int64{9, 3, 9, 5}
	out, err := in.Normalize()
	if err != nil || !reflect.DeepEqual(out.ProjectIDs, []int64{3, 5, 9}) {
		t.Fatalf("%v %v", out.ProjectIDs, err)
	}
	in.ProjectIDs = []int64{0}
	if _, err := in.Normalize(); err == nil {
		t.Error("a non-positive project id was accepted")
	}
}

func TestServesProject(t *testing.T) {
	all := App{ProjectScope: ScopeAll}
	if !all.ServesProject(42) {
		t.Error("scope all must serve any project")
	}
	some := App{ProjectScope: ScopeSelected, ProjectIDs: []int64{1, 2}}
	if !some.ServesProject(2) || some.ServesProject(3) {
		t.Error("selected scope must serve only its set")
	}
}

func TestSeedKey(t *testing.T) {
	for key, ok := range map[string]bool{"corp": true, "corp-2": true, "": false, "-x": false, "Corp": false, strings.Repeat("a", 41): false} {
		if ValidSeedKey(key) != ok {
			t.Errorf("ValidSeedKey(%q) = %v", key, !ok)
		}
	}
}

func TestAWhitespaceOnlySecretIsRefusedWhileAnExplicitEmptyOneClears(t *testing.T) {
	blank := validInput()
	blank.Secret = secretPtr("   \t ")
	var validation ValidationError
	if _, err := blank.Normalize(); !errors.As(err, &validation) || validation.Field != "secret" {
		t.Errorf("a blank secret: %v", err)
	}
	clear := validInput()
	clear.Secret = secretPtr("")
	out, err := clear.Normalize()
	if err != nil || out.Secret == nil || *out.Secret != "" {
		t.Errorf("an explicit empty secret must stay a clear: %v %v", out.Secret, err)
	}
}

func TestATenantLongerThanTheMigrationAllowsIsRefusedBeforeTheInsert(t *testing.T) {
	long := "https://login.microsoftonline.com/" + strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 60) + "." + strings.Repeat("e", 60) + ".com"
	var validation ValidationError
	if _, err := ParseAuthority(long); !errors.As(err, &validation) || validation.Field != "authority" {
		t.Errorf("a 250+ character domain: %v", err)
	}
}

func TestNormalizeIDAcceptsOnlyTheHyphenatedGuid(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, spelling := range []string{"urn:uuid:" + id, "{" + id + "}", strings.ReplaceAll(id, "-", ""), "", "x"} {
		if _, ok := NormalizeID(spelling); ok {
			t.Errorf("%q was accepted", spelling)
		}
	}
	if got, ok := NormalizeID(" " + strings.ToUpper(id) + " "); !ok || got != id {
		t.Errorf("canonical form = %q %v", got, ok)
	}
}

func TestADateOnlyExpiryIsTheEndOfThatDay(t *testing.T) {
	got, err := ParseExpiry("2027-06-30")
	if err != nil || got.Format("2006-01-02T15:04:05Z") != "2027-06-30T23:59:59Z" {
		t.Fatalf("date = %v %v", got, err)
	}
	// The last valid day is not expired during it, and is expired the day after.
	if state, _ := SecretExpiry(&got, time.Date(2027, 6, 30, 12, 0, 0, 0, time.UTC)); state == ExpiryExpired {
		t.Error("a secret that expires on 30 June was expired on 30 June")
	}
	if state, _ := SecretExpiry(&got, time.Date(2027, 7, 1, 0, 0, 1, 0, time.UTC)); state != ExpiryExpired {
		t.Errorf("1 July = %s", state)
	}
	if parsed, err := ParseExpiry("2027-06-30T10:00:00Z"); err != nil || parsed.Hour() != 10 {
		t.Errorf("a full time is kept: %v %v", parsed, err)
	}
	if _, err := ParseExpiry("soon"); err == nil {
		t.Error("garbage was accepted")
	}
}
