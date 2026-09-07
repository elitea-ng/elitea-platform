// Package toolkitcalltool is the elitea-main producer for the worker's
// `toolkit.call_tool.v1` capability: run ONE tool of ONE saved toolkit and
// answer the caller with what it returned (issues #340 and #616).
//
// It is the second half of a capability whose first half already shipped. The
// proto command, the Python worker handler, the Rust worker's typed refusal and
// the kernel's capability acceptance (shared migration 0115) all landed before
// this package existed, and until it existed nothing in this platform could
// produce a tool-run command at all. That is why
// `POST .../test_tool/...` answered `503 indexer service not available`.
//
// # THE ROUTE DECISION (#340, recorded here because it has no other home)
//
// Two handlers claim `POST /api/v2/elitea_core/test_toolkit_tool/prompt_lib/{projectID}`:
//
//	router.go:2385                    toolkits.Handler.TestToolkitTool   (503)
//	production_router.go:157          indexing.StartHandler.Start        (index_data only)
//
// `indexingapi.CurrentIndexStartPath` is that exact string, and chi resolves an
// explicitly registered path before the wildcard node a `Route()` subrouter
// mounts. So WHEREVER THE RUNTIME IS COMPOSED — every deployment that sets
// ELITEA_RUNTIME_ENABLED, which is every deployment that can run a worker at
// all — `StartHandler.Start` wins and `TestToolkitTool` is unreachable. Where
// the runtime is NOT composed, `CurrentIndexStart` is nil, the production
// registration is skipped, and `TestToolkitTool` is the only handler there is.
//
// Both are therefore real, in different deployments, and the decision is:
//
//  1. `StartHandler.Start` owns the SYNCHRONOUS branch of that path. It already
//     refuses every request that is not `index_data` with `await_response=false`
//     (start_handler.go:55,87); those two refusals become the entry to this
//     package. This is the branch that matters, because it is the reachable one
//     wherever a worker exists to run the tool.
//  2. `toolkits.Handler.TestToolkitTool` and `toolkits.Handler.TestTool` keep
//     their own registrations and call the SAME use case when it is composed.
//     `TestTool` at `/test_tool/prompt_lib/{projectID}/{toolID}` is shadowed by
//     nothing and is reachable in every deployment.
//  3. Neither is deleted. The 503 stays as the answer when the use case is
//     absent, which is exactly what a deployment with no runtime should say.
//
// NOTE(#340): no test asserts `TestToolkitTool` is dead, and it is not — it is
// live in a runtime-less deployment. It is left standing deliberately.
//
// # WHY THERE IS NO BINDING TABLE
//
// `index.ingest.v1` and `agent.execute.*.v1` each own a per-capability row
// (`index_ingest_jobs`, `agent_execution_jobs`) because a BACKGROUND publisher
// rebuilds their worker command from the database, minutes after admission. A
// tool run is admitted and dispatched inside one bounded synchronous request,
// so its command scalars never leave the process that built them. The two input
// entry ids are recovered from `input_bundle_entries.semantic_role`, which is
// durable already. See internal/db/queries/runtime_toolkit_call_tool.sql for
// what that costs and how the cost is bounded.
//
// # WHAT RIDES WHERE
//
// The toolkit settings and the caller's arguments are TWO separate input-bundle
// entries with distinct semantic roles, never one blob and never on the command.
// The settings are redeemed by this service from the saved toolkit row and hold
// credentials; the arguments are caller content. Binding them apart is what
// lets the worker refuse a bundle that put caller content where the platform's
// own settings belong — a refusal that exists in four places and would be
// unenforceable if the two shared an entry.
package toolkitcalltool
