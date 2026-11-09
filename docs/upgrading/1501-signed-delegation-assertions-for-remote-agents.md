---
title: "Signed delegation assertions for remote agents (#1501)"
issue: 1501
date: 2026-10-30
order: 52
---

ADR-0044 decision 4. Shared migration **0210_remote_agent_assertions** adds
`elitea_identity.remote_agent_assertion_replays` (the one-time record of
inbound assertions, bound to the credential that first presented each one;
rows are deleted after they expire). No permission and no data.

- **Upgrade order: Main, then the workers** (the chart's default order, and
  the order the delegation narrowing entry above also uses). The runtime
  control service has a new RPC, `MintDelegationAssertion`. A new worker asks
  Main for an assertion before each remote MCP call; with Main upgraded first,
  the RPC is there when the first new worker starts. While the workers roll
  out, an older worker never asks, so its calls carry no assertion. A worker
  upgraded before Main (outside the chart's order) gets `UNIMPLEMENTED`: a
  governed run then refuses its remote MCP calls
  (`agent_assertion_unavailable`), and any other run calls without an
  assertion.
- **Signing key.** Assertions are signed with the agent assertion key ring of
  `ELITEA_AGENT_ASSERTION_CONFIG`. Main now loads that ring whenever the
  variable is set, also with no relying parties (it still does not in a
  governed deployment whose revision does not list `elitea_native`), and then
  serves `/.well-known/elitea-agent-jwks.json`. Without the variable, nothing
  changes for runs that are not governed: Main answers "disabled" after one
  run read (no settings read, no toolkit walk, no audit row), the worker
  remembers that answer for the run, and no header is sent. A governed run
  needs an assertion for each remote MCP call: configure the key before you
  upgrade the worker, or governed runs refuse their remote MCP calls
  (`agent_assertion_unavailable`).
- **Setting.** Configuration › Remote Agents (`remote_agents` section, gated
  on `configuration.governance`): `outbound_assertions` = `off` (default:
  nothing is sent to a third party), `allowlist` (only to the origins in the
  new `assertion_audiences` list, at most 256) or `on` (every remote MCP
  server); a governed run always sends one. The assertion carries the user,
  project, run, mandate and toolkit types of the run. `trusted_issuers`: entries
  `<issuer>` or `<issuer> <jwks_url>`, https only, at most 32, empty by
  default. Helm bootstrap: `main.env.ELITEA_REMOTE_AGENT_TRUSTED_ISSUERS`
  (comma or new line separated, added to the stored list; a malformed entry
  refuses boot).
- **Outbound.** Remote MCP calls that the setting (or governance) selects
  carry `Elitea-Agent-Assertion: <jwt>` (typ `elitea-delegation+jwt`,
  5-minute lifetime, cached per endpoint; a failed mint is not asked again
  for 30 seconds). The worker sends the endpoint as configured and Main
  normalizes it. Main refuses to sign for the audience of a federation
  relying party.
- **Inbound.** The MCP endpoint verifies the header when it is present (401
  `agent_assertion_refused` with a code, 503 when the check cannot be done)
  and limits every agent and toolkit tool called with it to the asserted
  toolkits (platform-internal tools are refused to such a caller). A JWKS
  that cannot be fetched answers 503 `agent_assertion_unavailable`.
  **Governed profile:** invoking an agent through MCP with a personal access
  token or a browser session alone is now refused
  (`agent_invocation_requires_agent_credential`); give agent callers an OAuth
  client. An OAuth caller without an assertion gets its task framed as
  untrusted data (governed profile only).
- **Audit.** New security event type
  `ai.elitea.security.v1.remote_agent.assertion`.
