# Worker logs: `failure_diagnostic` reads `not_captured`, and undeclared tool calls are logged by name (#3050)

**What changed.** The Rust worker's failure log lines (`native agent event
stream failed`, `nested_application_failed`) carry `failure_diagnostic`, the
opt-in capture that `ELITEA_RUST_FAILURE_DIAGNOSTICS=on` turns on. When no
capture was taken (the setting is off, or its one-per-second allowance was
spent), the field used to read `disabled_or_rate_limited`. That value says
nothing about the failure, but it read like a cause: an agent turn that failed
on `model_facade.tool_not_admitted` looked rate limited. The field now reads
`not_captured`. The failure's class is in `error_code` and
`upstream_error_code`, as before.

A model's call to a tool the request does not declare is now logged with the
tool's name (letters, digits and `_.:-` only, at most 64 characters):
`model_tool_not_admitted` when it is refused before any output (the worker asks
the model once more), and `model_tool_not_admitted_dropped` when it is dropped
after the answer streamed (the turn ends on the answer and a note). An
unrepaired refusal now ends the run as `MODEL_RESPONSE_INVALID`, not
`INTERNAL`.

**Who is affected.** Operators with log queries, alerts or dashboards that
match `failure_diagnostic=disabled_or_rate_limited`, or that count
`upstream_error_code=model_facade.tool_not_admitted` under `INTERNAL`.

**What to do.** Change such queries to match `not_captured`, and count
`model_tool_not_admitted*` events for undeclared tool calls. Nothing else
changes.
