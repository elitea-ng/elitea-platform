---
date: 2026-10-30
order: 3
---
## Delegation narrowing

When an agent calls a saved agent or pipeline, a pipeline node runs one, or an agent starts a sub-agent, the delegated agent now runs with only the toolkits, tools, credentials, data classes and egress hosts that its caller also holds. A delegation that needs more is refused with `DELEGATION_GRANT_MISSING`, which names the missing grant. The model reads the refusal, and the run does not fail. **Configuration › Delegation** sets the maximum delegation depth (default 4) and a `report` mode that records missing grants without refusing them. Save it after every worker runs this release; data class narrowing starts when it is saved. Each delegation step records its depth and a digest of its grants. **Compositions deeper than 4, and callees that hold toolkits their callers do not hold, are refused after the upgrade.** See [Delegation narrowing](/admin/enterprise-controls/least-privilege-and-trust#delegation-narrowing).
