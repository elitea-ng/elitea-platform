package analytics

import "strings"

// InferenceRouteSQLList is the SQL value list of the gateway routes whose
// requests count as MODEL CALLS on the analytics pages (legacy issue 6879).
// Use it as `route IN (` + InferenceRouteSQLList + `)`.
//
// gateway.llm_request_logs holds one row per request the gateway served, and
// the gateway serves more than model calls. It also logs the model listing, the
// Anthropic token counter, the operator's connection check, the provider
// catalogue reads, and every request that matched no route. None of those
// invokes a model for the project, and counting them made the Analytics
// "LLM calls" figure disagree with the Usage page, which reads the billing
// ledger.
//
// The list is the CANONICAL definition of a model call: one invocation of a
// model that does or can produce usage, whatever the model family. Embeddings,
// image generation and audio are model calls and are on it.
//
// The values are the chi route PATTERNS the gateway's request log stores
// (services/elitea-llm-gateway/internal/api/router.go), never raw URLs. A route
// the gateway adds later is NOT counted until it is added here. Two tests stop
// that from happening silently. The gateway's route_classes_test.go fails when
// it mounts a route that services/elitea-llm-gateway/internal/api/testdata/
// llm_route_classes.txt does not class, and
// TestInferenceRoutesMatchTheGatewayRouteClasses fails when this list and the
// "inference" lines of that file differ.
//
// It is a CONSTANT so the analytics statements that embed it stay constants.
// The values are literals in the statement text rather than bound parameters,
// so every statement keeps its parameter list; no caller value reaches this
// text. TestInferenceRouteSQLListIsWellFormed holds the quoting.
const InferenceRouteSQLList = `'/llm/v1/chat/completions', '/llm/v1/completions', ` +
	`'/llm/v1/embeddings', '/llm/v1/responses', '/llm/v1/messages', ` +
	`'/llm/v1/images/generations', '/llm/v1/images/edits', '/llm/v1/images/variations', ` +
	`'/llm/v1/audio/speech', '/llm/v1/audio/transcriptions', '/llm/v1/audio/translations', ` +
	`'/llm/v1/realtime'`

// CompletedStatusSQL is the status predicate of a COMPLETED model call: a
// request the gateway answered below 400. Use it as `AND ` + CompletedStatusSQL,
// or with the `l.` alias as `AND l.` + CompletedStatusSQL.
//
// The call, token, user and cost figures count completed calls only. The Usage
// page reads the billing ledger, and the ledger records a call only when the
// gateway billed it. A budget refusal (402), a rate limit (429), a validation
// error (400) or a provider failure (5xx) is not billed. Counting those rows
// as calls made Analytics disagree with Usage in the same way issue 6879
// reported: a project that retries against its budget limit 20 times showed
// 20 more calls than Usage.
//
// The agent breakdown and the Health tab do NOT use this predicate. They count
// ATTEMPTS, because they show errors, and an error is a request that did not
// complete. See internal/infra/db/repos/analytics.go.
const CompletedStatusSQL = `status < 400`

// InferenceRoutes returns the routes of InferenceRouteSQLList as plain strings.
func InferenceRoutes() []string {
	parts := strings.Split(InferenceRouteSQLList, ",")
	routes := make([]string, 0, len(parts))
	for _, part := range parts {
		routes = append(routes, strings.Trim(strings.TrimSpace(part), "'"))
	}
	return routes
}
