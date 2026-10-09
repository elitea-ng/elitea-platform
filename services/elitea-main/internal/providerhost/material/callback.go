package material

// The callback credential: minted for one invocation, revoked when the hop
// refuses it, and carried to the engine in the one block the engine reads for
// BOTH its model calls and its artifact uploads.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Minter mints the short-lived, project-bound bearer the provider calls back
// with. Satisfied by v2auth.CallbackTokenMinter through each facade's adapter;
// narrowed to what a rewrite uses so a facade's tests need no database.
type Minter interface {
	Mint(ctx context.Context, ownerID, projectID int64, name string, lifetime time.Duration) (Grant, error)
	Revoke(ctx context.Context, ownerID int64, tokenUUID string) error
}

// Grant is one minted bearer.
type Grant struct {
	Bearer  string
	Expires time.Time
	UUID    string
}

// CallbackSettings builds the `llm_settings` block.
//
// One block, two uses, and that is the engine's design rather than a shortcut
// here: extract_artifact_settings derives the artifact API base by STRIPPING
// `/llm/v1` off api_base and takes project_id from `organization`. Splitting
// them would mean editing the engine, which this port does not do.
//
// model is a caller choice, not a credential, so the caller's own model name
// still decides it. Anything else the client put in llm_settings is discarded
// with the block.
func CallbackSettings(base string, grant Grant, projectID int64, model string) map[string]any {
	settings := map[string]any{
		"api_base": base + "/llm/v1",
		"api_key":  grant.Bearer,
		// The engine reads project_id from `organization` first. It is the
		// project the artifacts land in and the project the model calls bill,
		// and both must be the one in the path — not one the client names.
		"organization": fmt.Sprintf("%d", projectID),
	}
	if grant.UUID != "" {
		// The run the gateway attributes this invocation's model spend to,
		// sent by the engine as X-Elitea-Execution-Id (the /llm caller
		// contract, as the worker sends its execution id). The /llm edge
		// keeps it only while it names the very token that authenticated the
		// call (llmproxy.CallbackExecutionPrefix), so it cannot be borrowed.
		settings["execution_id"] = CallbackExecutionPrefix + grant.UUID
	}
	if model != "" {
		settings["model_name"] = model
	}
	return settings
}

// CallbackExecutionPrefix is llmproxy.CallbackExecutionPrefix; the edge
// recognises a provider invocation's execution id by it. Kept as its own
// constant so this package stays free of the edge (TestCallbackExecutionPrefix
// pins the two together).
const CallbackExecutionPrefix = "callback-"

// ToolSettingsCarried are the keys of a TOOL-level llm_settings block a facade
// carries into its own: tuning, never transport. Everything else in that block
// is dropped with it.
//
// reasoning_effort is tuning too, and the engines' model client already
// reads it (libs/rust/model-client settings.rs): without it a reasoning model
// (Qwen3 on vLLM, measured) thinks for thousands of tokens per extraction
// call and an ingestion of a 50-file repository outlives the gateway's
// timeout. `none` turns thinking off where the gateway passes it through.
var ToolSettingsCarried = []string{"max_tokens", "temperature", "reasoning_effort"}

// LiftToolLLMSettings removes a client's llm_settings from the TOOL-level
// `parameters` and lifts its tuning keys into the facade's block.
//
// The provider merges the tool's own parameters OVER the configuration's (the
// legacy `if key not in params or value` rule, kept by the host), so a
// tool-level `llm_settings` — which the wiki chat sends, carrying max_tokens
// and the model — replaced the facade's block wholesale and the engine refused
// with `llm_settings.api_base is required`. MEASURED on the first real-engine
// chat through the product (DWIKI-014b). The same hole let a client push
// api_base/api_key past the rewrite that exists to stop that: the
// configuration-level block was replaced, the tool-level one merged.
func (e *Envelope) LiftToolLLMSettings(block map[string]any) error {
	// The toolkit's own saved default goes in first, so a caller's value,
	// lifted below, wins over it.
	if effort := e.toolkitReasoningEffort(); effort != "" {
		block["reasoning_effort"] = effort
	}
	encoded, ok := e.root["parameters"]
	if !ok || IsNull(encoded) {
		return nil
	}
	tool := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &tool); err != nil {
		return fmt.Errorf("%w: parameters is not an object", ErrRejected)
	}
	raw, ok := tool["llm_settings"]
	if !ok {
		return nil
	}
	delete(tool, "llm_settings")
	if !IsNull(raw) {
		var client map[string]any
		if err := json.Unmarshal(raw, &client); err != nil {
			return fmt.Errorf("%w: parameters.llm_settings is not an object", ErrRejected)
		}
		for _, key := range ToolSettingsCarried {
			if v, ok := client[key]; ok && v != nil {
				block[key] = v
			}
		}
	}
	rewritten, err := json.Marshal(tool)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrRejected, err)
	}
	e.root["parameters"] = rewritten
	return nil
}

// toolkitReasoningEffort reads the toolkit-level default from the saved
// configuration: the flat `configuration.parameters.reasoning_effort` the
// Inventory toolkit form writes, then the nested
// `configuration.parameters.llm_settings.reasoning_effort` an agent version
// stores. Only values the model client accepts count; anything else is
// ignored. No other toolkit-level setting is carried.
func (e *Envelope) toolkitReasoningEffort() string {
	if effort := validReasoningEffort(e.parameters["reasoning_effort"]); effort != "" {
		return effort
	}
	raw, ok := e.parameters["llm_settings"]
	if !ok || IsNull(raw) {
		return ""
	}
	var saved map[string]json.RawMessage
	if json.Unmarshal(raw, &saved) != nil {
		return ""
	}
	return validReasoningEffort(saved["reasoning_effort"])
}

// validReasoningEffort returns the value when it is one the Rust model client
// and the gateway accept (libs/rust/model-client ReasoningEffort), else "".
func validReasoningEffort(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	switch value {
	case "none", "low", "medium", "high":
		return value
	}
	return ""
}

// Revoke gives a grant back, and never fails a request for it.
func Revoke(
	ctx context.Context,
	minter Minter,
	logger *slog.Logger,
	ownerID int64,
	tokenUUID string,
) {
	if minter == nil || tokenUUID == "" {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	// context.WithoutCancel: the request context may already be done — a
	// client that disconnected is one of the cases that leaves a token behind
	// — and revoking is exactly the work that must still happen then.
	if err := minter.Revoke(context.WithoutCancel(ctx), ownerID, tokenUUID); err != nil {
		logger.Warn("provider callback token not revoked; it expires on its own",
			"token", tokenUUID, "error", err)
	}
}
