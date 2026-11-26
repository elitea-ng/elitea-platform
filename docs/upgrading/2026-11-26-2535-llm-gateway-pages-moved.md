# Admin console: the LLM gateway tabs are pages of Models and spend (#2535, ADR-0059 R3.5a)

**What changed.** The seven tabs of **Configuration › LLM Proxy** are pages of
the **Models and spend** area, beside LLM Governance and Budgets:

| Former tab | Page | Address under `/admin/app` |
|---|---|---|
| Providers & models | Providers and models | `/models/providers` |
| Models & pricing | Models and pricing | `/models/pricing` |
| Tools | Tool prices | `/models/tool-prices` |
| Usage | Usage | `/models/usage` |
| Status | the Status tab of Usage | `/models/status` |
| Logs | Logs | `/models/logs` |
| Budget alerts | the Budget alerts tab of Budgets and alerts | `/budgets/alerts` |

The Budgets item is now called **Budgets and alerts**. The panels, their
permissions and the API routes they call are unchanged.

**Who is affected.** Administrators with bookmarks. With the six labelled areas
on (`admin_console.admin_console_areas`), the **LLM Proxy** row of Configuration
opens Providers and models, `/admin/app/configuration#llm_proxy` redirects
there, and `#llm_proxy/<tab>` redirects to that tab's page (`status`,
`providers`, `models`, `tools`, `usage`, `logs`, `alerts`). With the areas off
(an upgraded deployment, until an administrator turns them on), Configuration
still shows the seven tabs; the new addresses work as well.

**What to do.** Nothing is required. Update bookmarks to the new addresses when
convenient.
