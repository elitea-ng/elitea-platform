// model_connection_check.go is the llm_model branch of
// POST /configurations/check_connection/{projectID}/llm_model (legacy tracker
// issue 6793: "Test connection in the LLM model form").
//
// The credential test answers "does this key authenticate?". It reports
// success for a wrong model name and for a wrong DIAL API protocol, so an admin
// learns about either mistake only at the first chat message. This branch
// tests the MODEL instead, with the values in the form, saved or not:
//
//  1. Read the model name, the AI credentials reference and the API protocol
//     from the posted form. Nothing else of the form is used.
//  2. Resolve the referenced credential ON THE SERVER, through the same
//     expander and vault the runtime uses. The browser never holds the key,
//     and it cannot add fields to the credential: the reference is rebuilt
//     from its title and private flag alone, so a posted api_base cannot
//     redirect a redeemed key.
//  3. Ask the gateway for one real completion with a one-token budget. The
//     gateway owns the egress allowlist and the address-validating dialer
//     (issue 13), so this process never dials the provider.
//
// The test saves nothing and changes nothing.
package configurations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
)

const llmModelConfigurationType = "llm_model"

// modelConnectionCheckClientTimeout is how long this process waits for the
// gateway's model probe. The gateway bounds the probe at 30 s; the extra time
// covers the hop itself, so a provider timeout arrives as the gateway's
// "timeout" verdict and not as a transport error here.
const modelConnectionCheckClientTimeout = 40 * time.Second

// modelConnectionMaxResponseBytes bounds the gateway reply this process reads.
const modelConnectionMaxResponseBytes = 64 << 10

// gatewayModelProbeKind is the gateway's marker for a model probe answer. A
// gateway that predates the model probe ignores `model` and answers the
// credential listing instead; that answer must never read as "Connected".
const gatewayModelProbeKind = "completion"

// ModelConnectionCheck is one llm_model test: the RESOLVED credential and the
// form's model values.
type ModelConnectionCheck struct {
	CredentialType string
	// Credential is the plaintext credential payload. It goes to the gateway
	// and is dropped. It is never logged, stored or returned.
	Credential  map[string]any
	Model       string
	APIProtocol string
}

// ModelConnectionResult is the gateway's verdict on one model test.
type ModelConnectionResult struct {
	Success bool
	// Reason is the gateway's machine-readable reason ("ok" on success).
	Reason string
	// Message is safe for the browser: a fixed category, then the provider's
	// own message after the gateway scrubbed it.
	Message   string
	LatencyMS int64
}

// ModelConnectionChecker tests one model through the gateway.
//
// Implementations MUST NOT dial a provider directly and MUST NOT report
// Success unless the model answered a completion.
type ModelConnectionChecker interface {
	CheckModel(ctx context.Context, check ModelConnectionCheck) (ModelConnectionResult, error)
}

var _ ModelConnectionChecker = (*GatewayConnectionChecker)(nil)

// modelConnectionCategories maps the gateway's reasons onto the six
// categories the form shows.
var modelConnectionCategories = map[string]string{
	"unauthorized":       "Authentication failed",
	"model_not_found":    "Model not found",
	"protocol_error":     "Wrong API protocol or route",
	"timeout":            "Timed out",
	"rate_limited":       "Rate limited",
	"unreachable":        "Connection failed",
	"upstream_error":     "Connection failed",
	"egress_not_allowed": "Connection failed",
}

// modelConnectionMessageFor builds the browser message for a failed test.
func modelConnectionMessageFor(reason, detail string) string {
	category, known := modelConnectionCategories[reason]
	if !known {
		// The credential's own field problems (no api_base, a bad region)
		// keep the credential test's wording.
		return connectionCheckMessageFor(reason, detail)
	}
	switch {
	case reason == "egress_not_allowed":
		detail = connectionCheckMessages[reason]
	case reason == "timeout" && detail == "":
		detail = "The model did not answer within 30 seconds."
	}
	if detail == "" {
		return category
	}
	return category + ": " + detail
}

// CheckModel implements ModelConnectionChecker over the same mTLS hop and the
// same identity signature as Check.
func (c *GatewayConnectionChecker) CheckModel(ctx context.Context, check ModelConnectionCheck) (ModelConnectionResult, error) {
	if c == nil || c.modelHTTPClient == nil {
		return ModelConnectionResult{}, errors.New("check model connection: the gateway client is not composed")
	}
	body := gatewayCredentialRequestBody(check.CredentialType, check.Credential)
	body.Model = check.Model
	body.APIProtocol = check.APIProtocol
	raw, err := json.Marshal(body)
	if err != nil {
		return ModelConnectionResult{}, fmt.Errorf("check model connection: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/llm/v1/check_connection", bytes.NewReader(raw))
	if err != nil {
		return ModelConnectionResult{}, fmt.Errorf("check model connection: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	llmproxy.SignIdentityHeaders(req.Header, c.identitySecret, connectionCheckProjectIDFrom(ctx), "", "", "")

	resp, err := c.modelHTTPClient.Do(req)
	if err != nil {
		return ModelConnectionResult{}, fmt.Errorf("check model connection: call gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ModelConnectionResult{}, fmt.Errorf("check model connection: gateway responded with status %d", resp.StatusCode)
	}
	var out checkConnectionResponseBody
	if err := json.NewDecoder(io.LimitReader(resp.Body, modelConnectionMaxResponseBytes)).Decode(&out); err != nil {
		return ModelConnectionResult{}, fmt.Errorf("check model connection: decode gateway response: %w", err)
	}
	if out.Probe != gatewayModelProbeKind {
		return ModelConnectionResult{}, errors.New("check model connection: the gateway did not run a model probe")
	}
	if out.Success {
		return ModelConnectionResult{Success: true, Reason: out.Reason, Message: "Connected", LatencyMS: out.LatencyMS}, nil
	}
	return ModelConnectionResult{
		Reason: out.Reason, Message: modelConnectionMessageFor(out.Reason, out.Detail), LatencyMS: out.LatencyMS,
	}, nil
}

// gatewayCredentialRequestBody copies the credential fields the gateway reads
// into its wire body. It matches the field set Check sends.
func gatewayCredentialRequestBody(configType string, data map[string]any) checkConnectionRequestBody {
	return checkConnectionRequestBody{
		Type:               configType,
		APIBase:            strVal(data, "api_base"),
		APIKey:             firstStrVal(data, "api_key", "api_token"),
		APIVersion:         strVal(data, "api_version"),
		AWSAccessKeyID:     strVal(data, "aws_access_key_id"),
		AWSSecretAccessKey: strVal(data, "aws_secret_access_key"),
		AWSSessionToken:    strVal(data, "aws_session_token"),
		AWSRegionName:      strVal(data, "aws_region_name"),
		VertexProject:      strVal(data, "vertex_project"),
		VertexLocation:     strVal(data, "vertex_location"),
		VertexCredentials:  data["vertex_credentials"],
	}
}

// modelConnectionChecker reads the model-test capability off the composed
// gateway client, for the reason providerModelLister gives: one client, one
// configuration, no second option for a composition root to forget.
func (h *Handler) modelConnectionChecker() ModelConnectionChecker {
	if h.connectionChecker == nil {
		return nil
	}
	checker, ok := h.connectionChecker.(ModelConnectionChecker)
	if !ok {
		return nil
	}
	return checker
}

// llmModelCheckInput is the part of a posted llm_model form the test uses.
type llmModelCheckInput struct {
	model       string
	credential  map[string]any
	apiProtocol string
}

// readLLMModelCheckInput reads the form values the test needs and says which
// required value is missing. The reference is REBUILT from its title and
// private flag: any other key the browser put beside them is dropped.
func readLLMModelCheckInput(data map[string]any) (llmModelCheckInput, string) {
	input := llmModelCheckInput{
		model:       strings.TrimSpace(strVal(data, "name")),
		apiProtocol: strings.ToLower(strings.TrimSpace(firstStrVal(data, "api_protocol", "dial_protocol"))),
	}
	reference, _ := data["ai_credentials"].(map[string]any)
	title := strings.TrimSpace(firstStrVal(reference, "elitea_title", "alita_title"))
	switch {
	case title == "":
		return input, "Select AI credentials before you test the connection."
	case input.model == "":
		return input, "Enter the model name before you test the connection."
	}
	private, _ := reference["private"].(bool)
	input.credential = map[string]any{"elitea_title": title, "private": private}
	return input, ""
}

// checkLLMModelConnection runs the llm_model test. The HTTP contract is the
// credential test's: 200 only when the model answered, 400 with
// {"success":false,"message":...} for every failure.
func (h *Handler) checkLLMModelConnection(w http.ResponseWriter, r *http.Request, projectID string, data map[string]any) {
	ctx := r.Context()
	input, missing := readLLMModelCheckInput(data)
	if missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": missing, "reason": "missing_fields"})
		return
	}
	checker := h.modelConnectionChecker()
	if checker == nil || h.storedResolver == nil {
		slog.ErrorContext(ctx, "check_connection: the llm_model test is not composed",
			"project_id", projectID, "checker", checker != nil, "resolver", h.storedResolver != nil)
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": storedConnectionCheckUnavailableMessage})
		return
	}
	owner, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || owner <= 0 || owner > math.MaxInt32 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "invalid project"})
		return
	}

	resolution := StoredConfigurationResolution{
		ProjectID: int32(owner),
		Data:      map[string]any{"ai_credentials": input.credential},
	}
	// A `private: true` reference names the CALLER's personal project, as it
	// does when the runtime resolves the saved model.
	if authorID, ok := currentConfigurationMutationAuthorID(ctx); ok && authorID > 0 {
		resolution.AuthorID = &authorID
	}
	resolved, err := h.storedResolver.ResolveStoredConfiguration(ctx, resolution)
	credential, _ := resolved["ai_credentials"].(map[string]any)
	if err != nil || credential == nil {
		slog.WarnContext(ctx, "check_connection: the llm_model credential did not resolve",
			"project_id", projectID, "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "The selected AI credentials could not be resolved. Check that they still exist and that their secret is set.",
		})
		return
	}
	credentialType := strVal(credential, "configuration_type")
	if _, checkable := checkableConnectionTypes[credentialType]; !checkable {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Testing a model is not supported yet for this credential type.",
			"reason":  ToolkitCheckReasonUnsupportedType,
		})
		return
	}
	// The guard runs on the RESOLVED credential: its api_base is the value
	// that reaches the provider.
	if err := validateNotSelfReferential(credential, selfLLMOrigins()); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": err.Error()})
		return
	}

	result, err := checker.CheckModel(WithConnectionCheckProjectID(ctx, projectID), ModelConnectionCheck{
		CredentialType: credentialType,
		Credential:     credential,
		Model:          input.model,
		APIProtocol:    input.apiProtocol,
	})
	if err != nil {
		slog.ErrorContext(ctx, "check_connection: llm_model gateway call failed",
			"project_id", projectID, "type", credentialType, "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "message": "Could not verify the connection right now. Please try again.",
		})
		return
	}
	if result.Success {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "message": result.Message, "latency_ms": result.LatencyMS,
		})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"success": false, "message": result.Message, "reason": result.Reason, "latency_ms": result.LatencyMS,
	})
}
