# Context policy delivery

Status: Main component implementation, 2026-09-17. [UI controls](context-policy-ui.md) have component/browser proof; deployed acceptance remains open.

## Functional source mapping

SDK revision: `18704a4070d098761fd1d35897dc53e412b4cbcc`.
Current compaction algorithms do not constrain the new design, as clarified by the user on 2026-09-17.
Other runtime features still require current-platform functional references.

| Reference contract | New owner |
| --- | --- |
| SDK `runtime/clients/client.py::_inject_summarization` receives conversation policy and separate summary controls. | Main `domain/contextsettings/runtime.go` creates the runtime projection and separate authored model selection. |
| Centry `elitea_core/utils/context_analytics.py::set_context_strategy` combines user defaults and conversation settings. | `contextsettings.Resolve` retains this precedence. New compaction uses Balanced or Full. |
| Centry `social/models/pd/users.py` stores context and summarization preferences. | Existing `UserContextDefaultsRepo` reads the authenticated actor's preferences. No schema change is required. |
| Current UI context controls expose settings and usage. | Preset controls and an accessible composer indicator are implemented. Accurate worker occupancy remains required integration work. Legacy numeric controls do not satisfy this contract. |

## Presets and output controls

New execution settings use `budget_mode: balanced` or `budget_mode: full`.
Balanced resolves against the model window, with a 272,000-token ceiling.
Full resolves against the authoritative model window.
Rust reserves the admitted output and margin inside either window.
The trigger remains 90 percent of usable input; the target remains 70 percent.

Legacy `max_context_tokens` values remain readable in product records but do not enter the new runtime settings projection.
This replaces the earlier plan to preserve those numbers as combined-window overrides.
It does not convert an old output limit into an input limit.
Model `max_tokens` remains an independent response cap and output reservation.
Existing frozen worker inputs retain their original interpretation during continuation and recovery.

The projection excludes display names, authored model maps, and fabricated model limits.
Main validates a summary selection through the authorized catalogue and delivers field 66 separately.
Stored JSON numbers are normalized before integer identity validation.
Disabled summarization requests no summary-model binding.

## Start, Regenerate, and Continue

`application/agentexecution/context_policy.go` connects the resolved strategy to the existing version freezer.
Application and ad-hoc Start paths both receive the policy.
Regenerate resolves current preferences for its new execution generation.
Continue restores the previously admitted settings and summary snapshot without reading newer preferences.

`infra/db/repos/agent_context_policy.go` reads the immutable input through existing execution and input-bundle bindings.
The query binds tenant, resource project, projection project, actor, conversation, response, and execution generation.
It checks the stored content digest and protobuf conversation/generation before returning a policy.
Missing or corrupt admitted input fails; it never silently substitutes current account defaults.
This source reads no worker checkpoint and creates no checkpoint ownership in Main.
Automatic crash recovery continues to use the existing immutable execution input.

The existing continuation path still resolves current task-model and toolkit contracts.
This slice freezes context policy and summary selection; it does not claim complete point 4 continuation parity.
Ordinary continuation and graph recovery still require their separate acceptance gates.

## Verification and delivery

- Main agent execution, context domain, conversation API, and runtime composition: 912 checks pass without skips.
- Two repository checks pass, including real PostgreSQL admitted-input lookup and rejection across project, actor, conversation, response, and generation boundaries.
- Digest corruption fails. Old empty context inputs remain readable.
- Go vet passes for all affected packages.

Tests use isolated temporary databases and do not modify product data.
Evidence uses the `elitea-point4-context-delivery-*` temporary log prefix.
The runtime status API does not invent preset capacity from legacy analytics; unknown capacity remains unavailable.
No migration, new table, or application database rewrite is required.

Deploy the compatible Rust worker before the updated Main.
Complete worker occupancy projection, summary quality, and deployed save/reload/model-loop browser tests before claiming acceptance.
The rehearsal deployment has not changed in this slice.
