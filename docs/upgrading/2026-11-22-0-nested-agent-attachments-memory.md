# Runtime: Main ships before Worker for nested saved-agent memory targets (FU-353)

Nested saved Agents (and nested saved Pipelines for `artifact` and `memory`
toolkits) can now use conversation attachments (under a strict monotonic caller
ceiling), bucket-pinned `artifact` toolkits, claim-bound `memory` toolkits, and
their own `agent_memory` (`recall`, `remember`, `forget`) tools.

- **What changed:** `elitea-worker-rust` sends optional `target_application_id`
  and `target_version_id` fields on `agent-memory/{remember,recall,forget}`
  requests made by nested saved Agents (omitted on root calls). `elitea-main`
  now accepts those fields, verifies bounded reachability from the frozen root
  input, checks retirement and folder visibility, forces `Attended = false`,
  and caps `write: "auto"` by `max(root_tier, target_tier)`. `elitea-main` also
  filters principal-hidden memory toolkits before vector claim admission.
- **Who is affected:** Deployments rolling out `elitea-main` and
  `elitea-worker-rust` independently where saved agents call nested saved
  agents that have `meta.memory` enabled.
- **What to do:** **Deploy `elitea-main` before `elitea-worker-rust`.**
  - *New Main + Old Worker:* fully compatible; root agent memory works as
    before and older Workers do not bind nested `agent_memory`.
  - *Old Main + New Worker:* root agent memory calls omit both target fields
    and continue working unchanged, but nested `agent_memory` tool calls are
    rejected by Old Main's strict JSON decoder (`DisallowUnknownFields`) and
    return `"memory_unavailable"` without falling back to root memory until
    `elitea-main` is upgraded.
