package workeregress

import (
	"slices"
	"strings"
	"testing"
)

// ADR-0057 decision 10: Outlook, Teams and Microsoft Graph call graph.microsoft.com
// only, and the worker never reaches the token endpoint, so a credential's
// discovery endpoint is no worker host. A Dataverse credential's own environment
// URL is its one host, and anything that is not an https .dynamics.com origin
// (sovereign clouds included) refuses the command.
func TestMicrosoftToolkitsOpenGraphAndTheDataverseOrgOnly(t *testing.T) {
	for _, toolkitType := range []string{"outlook", "teams", "ms_graph"} {
		hosts, err := HostsForToolkit(toolkitType, []byte(`{"x_configuration":{"oauth_discovery_endpoint":"https://login.microsoftonline.com/tenant","client_id":"c"}}`))
		if err != nil || !slices.Equal(hosts, []string{"graph.microsoft.com"}) {
			t.Fatalf("%s: %v %v", toolkitType, hosts, err)
		}
	}
	hosts, err := HostsForToolkit("dataverse", []byte(`{"dataverse_configuration":{"url":"https://contoso.crm4.dynamics.com/","tenant_id":"t"}}`))
	if err != nil || !slices.Equal(hosts, []string{"contoso.crm4.dynamics.com"}) {
		t.Fatalf("dataverse: %v %v", hosts, err)
	}
	// SharePoint keeps reading its discovery endpoint.
	hosts, err = HostsForToolkit("sharepoint", []byte(`{"sharepoint_configuration":{"oauth_discovery_endpoint":"https://login.microsoftonline.com/tenant"}}`))
	slices.Sort(hosts)
	if err != nil || !slices.Equal(hosts, []string{"graph.microsoft.com", "login.microsoftonline.com"}) {
		t.Fatalf("sharepoint: %v %v", hosts, err)
	}
	for _, bad := range []string{
		"https://contoso.crm.dynamics.cn", "https://contoso.crm.microsoftdynamics.us", "https://evil.example",
		"http://contoso.crm.dynamics.com", "https://contoso.crm.dynamics.com:8443", "https://contoso.crm.dynamics.com/api",
		"https://user@contoso.crm.dynamics.com", "https://dynamics.com",
	} {
		_, err := HostsForToolkit("dataverse", []byte(`{"dataverse_configuration":{"url":"`+bad+`"}}`))
		if _, refused := AsRefusal(err); !refused {
			t.Errorf("%s: err = %v, want a refusal", bad, err)
		}
	}
	// A refusal never echoes the credential's value.
	_, err = HostsForToolkit("dataverse", []byte(`{"url":"https://secret-org.example"}`))
	if refusal, ok := AsRefusal(err); !ok || strings.Contains(refusal.Message, "secret-org") {
		t.Fatalf("refusal = %v", err)
	}
}
