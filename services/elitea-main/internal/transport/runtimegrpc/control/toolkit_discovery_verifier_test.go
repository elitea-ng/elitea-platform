package control

import (
	"context"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestDiscoveryCommandVerifierAcceptsReferenceAndRefusesScopeConfusion(t *testing.T) {
	var command runtimev1.WorkerCommandV1
	require.NoError(t, proto.Unmarshal(validRawWorkerCommand(t), &command))
	command.CommandType = runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_AVAILABLE_TOOLS
	command.CapabilityId = "toolkit.available_tools.v1"
	command.ResourceClass = "agents"
	command.CapabilityCommand = &runtimev1.WorkerCommandV1_ToolkitAvailableTools{ToolkitAvailableTools: &runtimev1.ToolkitAvailableToolsCommandV1{ToolkitType: "mcp_records", SettingsEntryId: "toolkit-settings"}}
	verifier := newTestVerifier(t)
	raw, err := proto.Marshal(&command)
	require.NoError(t, err)
	_, err = verifier.Verify(context.Background(), signedEnvelope(raw))
	require.NoError(t, err)
	for name, mutate := range map[string]func(*runtimev1.WorkerCommandV1){
		"wrong command": func(c *runtimev1.WorkerCommandV1) {
			c.CommandType = runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL
		},
		"missing type":     func(c *runtimev1.WorkerCommandV1) { c.GetToolkitAvailableTools().ToolkitType = "" },
		"missing settings": func(c *runtimev1.WorkerCommandV1) { c.GetToolkitAvailableTools().SettingsEntryId = "" },
		"context collision": func(c *runtimev1.WorkerCommandV1) {
			c.GetToolkitAvailableTools().SettingsEntryId = "toolkit-runtime-context"
		},
		"foreign root": func(c *runtimev1.WorkerCommandV1) { c.RootExecutionId = "other" },
		"parent":       func(c *runtimev1.WorkerCommandV1) { c.ParentExecutionId = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := proto.Clone(&command).(*runtimev1.WorkerCommandV1)
			mutate(changed)
			raw, err := proto.Marshal(changed)
			require.NoError(t, err)
			_, err = verifier.Verify(context.Background(), signedEnvelope(raw))
			require.Error(t, err)
		})
	}
}
