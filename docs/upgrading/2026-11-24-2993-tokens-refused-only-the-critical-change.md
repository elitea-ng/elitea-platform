# Tokens are refused only the critical change on governance, provider and retention routes (#2993, ADR-0059)

**What changed.** This replaces the paragraph "Routes decided whole" of
[the critical-actions note](2026-11-24-2506-tokens-refused-on-critical-actions.md)
for three routes. Until now a personal access token with `scoped` or
`inherit` administration access was refused every change on them, because
the handlers could not tell a critical change from a harmless one. The
handlers now classify the change against the stored row and refuse the token
only the critical outcome, with the same `403` code
`token_critical_action_refused` and the same `admin.token_refused` audit
action:

| Route | Refused to a token | Allowed to a token |
|---|---|---|
| `POST /api/v2/admin/gateway/governance`, `PUT` and `DELETE .../governance/{id}` | a row of type `guardrail`; an update where either the stored row or the body is a guardrail | every other row type |
| `PUT /api/v2/admin/gateway/providers/{configID}` | a change of any field other than `label`, `display_name`, `description` and `tags` (the endpoint, the credential, the keyless identity, the type) | a change of only those four fields |
| `PUT /api/v2/admin/retention/policies/{dataClass}`, `PUT` and `DELETE .../projects/{projectID}` | a change that lets data be deleted sooner: a lower maximum, a maximum where data was kept forever, a lower minimum, or turning on project overrides (a stored override below the platform maximum then applies) | a change that keeps data as long or longer |

The comparison is with the stored row, read under the write's lock, so a
concurrent change cannot turn an allowed change into the critical one:

- A governance update or delete matches no stored guardrail for a token; the
  token gets the refusal instead of a write.
- A retention write reads the stored policy in its own transaction and
  classifies there.
- A global provider settings update by a token is held to the provider it
  read. When the provider changed in between, the update gets `409`: read it
  again and retry.

The list of critical actions in the earlier note is unchanged: the same
entries are critical, and the table of that note is still generated from the
registry.

**Who is affected.** Automation that used a personal access token on these
routes and got `token_critical_action_refused` for a harmless change (a
routing rule, a provider label, a longer retention) now gets the change.
Nothing that was refused before is allowed now except those harmless
changes.

**Rollback.** The previous release refuses a token every change on the
three routes again. Nothing is migrated.
