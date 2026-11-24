# Administration: the LLM gateway routes accept narrow permissions (#2492, ADR-0059)

**What changed.** One `configuration.governance` gate used to cover every route
under `/api/v2/admin/gateway`. Each sub-surface now has its own gate. Every gate
accepts a narrow permission or `configuration.governance`:

| Permission | Routes |
|---|---|
| `llm.catalogue.view` | Read global providers (`GET /providers`), platform models (`GET /platform_models`, `GET /platform_models/{id}/default_usage`), the default model (`GET /default_model`) and prices (`GET /models`, `GET /metered-prices`). |
| `llm.catalogue.manage` | Create, edit and delete them; set and clear the default model; `POST /providers/{id}/check`, `/revalidate` and `/models`. These three calls use the stored credential, so `llm.catalogue.view` does not reach them. |
| `llm.policy.view` | Read governance rules (`GET /governance`) and validate a CEL expression (`POST /governance/validate-cel`). |
| `llm.policy.manage` | Create, edit and delete routing rule, rate limit, budget and model config rules (`/governance`). |
| `llm.guardrails.manage` | Create, edit and delete guardrail rules (`/governance`). |
| `llm.usage.view` | Gateway status, usage, request logs and budget alerts (`GET /status`, `/usage`, `/logs`, `/budget-alerts`). |
| `llm.alerts.manage` | Save the budget alerts (`PUT /budget-alerts`). |

The governance rule routes decide by the kind of the rule. A guardrail write
needs `llm.guardrails.manage`, and `llm.policy.manage` is refused there. This
covers an edit or a delete of a stored guardrail too. Egress allowlist, MCP
allowlist and credential policy rules have no narrow permission yet and need
`configuration.governance`.

Shared migration `0261` (#2491) already granted the seven names to `super_admin`
and `admin`, so this release adds no migration. Project and member budgets keep
`models.admin.project_budgets.view` and `.edit`.

**Who is affected.** Nobody loses access. A holder of `configuration.governance`
sees no change: every route answers that holder as before. The built-in
`llm_manager` role (#2496) can now use the catalogue, the routing and throttle
rules, usage and alerts. It can read guardrails and cannot change them.

The navigation still names `configuration.governance` for these pages, so a role
with only narrow names reaches the routes but does not see the pages in the menu
yet. A later release changes the navigation.

**What to do.** Nothing.
