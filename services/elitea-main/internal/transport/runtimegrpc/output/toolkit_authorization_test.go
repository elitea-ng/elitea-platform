package output

import (
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestToolkitAuthorizationChallengeContract(t *testing.T) {
	original := &runtimev1.ToolkitCallToolSummaryV1{Status: runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_AUTHORIZATION_REQUIRED, ErrorMessage: executiondomain.ToolkitAuthorizationMessage, AuthorizationRequired: &runtimev1.ToolkitAuthorizationRequiredV1{ToolkitName: "saved", ToolkitType: "mcp", ToolkitId: "19", ServerUrl: "https://mcp.example.test/", ResourceMetadataJson: []byte(`{"authorization_servers":["https://login.example.test"],"oauth_authorization_server":{"authorization_endpoint":"https://login.example.test/auth","token_endpoint":"https://login.example.test/token"}}`)}}
	if _, err := toolkitCallToolSummaryDomain(original); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*runtimev1.ToolkitCallToolSummaryV1){
		"wrong status": func(s *runtimev1.ToolkitCallToolSummaryV1) {
			s.Status = runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_OK
		},
		"missing challenge": func(s *runtimev1.ToolkitCallToolSummaryV1) { s.AuthorizationRequired = nil },
		"provider result":   func(s *runtimev1.ToolkitCallToolSummaryV1) { s.ResultJson = `{"token":"secret"}` },
		"raw error":         func(s *runtimev1.ToolkitCallToolSummaryV1) { s.ErrorMessage = "provider secret" },
		"credential URL": func(s *runtimev1.ToolkitCallToolSummaryV1) {
			s.AuthorizationRequired.ServerUrl = "https://user:secret@example.test/"
		},
		"metadata token": func(s *runtimev1.ToolkitCallToolSummaryV1) {
			s.AuthorizationRequired.ResourceMetadataJson = []byte(`{"access_token":"secret"}`)
		},
		"foreign metadata toolkit": func(s *runtimev1.ToolkitCallToolSummaryV1) {
			s.AuthorizationRequired.ResourceMetadataJson = []byte(`{"toolkit_id":"20"}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			value := proto.Clone(original).(*runtimev1.ToolkitCallToolSummaryV1)
			mutate(value)
			if _, err := toolkitCallToolSummaryDomain(value); err == nil {
				t.Fatal("invalid challenge accepted")
			}
		})
	}
}
