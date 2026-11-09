---
title: "elitea-main: governed credential broker (off by default)"
issue: 1463
date: 2026-10-27
order: 31
---

**Affects:** nobody has to act while
`ELITEA_ENTERPRISE_CREDENTIAL_GOVERNANCE_ENABLED` stays `false`. One
additive shared migration (0188) creates three empty tables:
`bound_tool_invocations`, `credential_redemptions` and
`execution_agent_sessions`.

**What changed.**

* `RedeemCredential` on the runtime content service is the only source of
  toolkit credentials for a governed run (#1118). With the flag off it
  answers `UNIMPLEMENTED` and no request path runs a new statement.
* With the flag on, `GetEliteaClientToken` serves a governed run its token
  for the model path but no project secrets header value. There is no
  `GOVERNED_PROFILE_NO_CLIENT_TOKEN` refusal. A legacy run is unchanged.
* An autonomous mandate's run is decided with the permissions of the user who
  approved the mandate, read at each decision, and audited as attributed.
* The broker reads Vault/OpenBao as the workload scope `credential-broker`.
  If a revision requires Vault, create the role `<prefix>-credential-broker`.

**Still not enforced end to end.** The operation catalogue is empty until the
provider adapters ship, so every redemption is denied. Do not move toolkits
that need a credential to a governed deployment yet. The operator guide
(`/docs/deployment/credential-governance`) lists the denial codes.
