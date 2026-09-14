package repos

import (
	"context"
	"fmt"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

// These terminal results have no projection outside the durable inbox. Older
// writers omitted the marker. Complete it in the claim transaction so existing
// settlement recovery can reuse the result without another provider call.
// Other payload types must finish their own product projection first.
func completeToolkitInboxProjection(ctx context.Context, tx sqlExecutor, request executionapp.ClaimRequest) error {
	var payloadType string
	switch request.CapabilityID {
	case executiondomain.ToolkitCallToolCapability:
		payloadType = payloadTypeToolkitCallToolResult
	case executiondomain.ToolkitAvailableToolsCapability:
		payloadType = payloadTypeToolkitAvailableToolsResult
	default:
		return nil
	}
	_, err := tx.Exec(ctx, `
UPDATE elitea_runtime.output_inbox
SET projected_at = clock_timestamp()
WHERE execution_id = $1 AND generation = $2
  AND payload_type = $3 AND projected_at IS NULL`, request.ExecutionID, int64(request.Generation), payloadType)
	if err != nil {
		return fmt.Errorf("complete toolkit inbox projection: %w", err)
	}
	return nil
}
