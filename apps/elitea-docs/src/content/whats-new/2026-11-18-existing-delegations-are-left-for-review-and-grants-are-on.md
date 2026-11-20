---
date: 2026-11-18
order: 7
---
## Existing delegations are left for review, and grants are on by default

When you upgrade, each attached agent that holds more than the agent that attaches it gets a **pending** delegation grant for the owners to review: nothing is approved and no agent can reach more than before. The calling agent's owners and editors get one notification, the agent's Tools panel shows a banner with **Review delegation**, and **Admin › Delegations awaiting review** lists the queue per project. Delegation grants and the run principal bound are now on by default; a deployment with no saved mode enforces, and **Mode** `report` for about 14 days keeps those calls running while owners review. Pipeline edges defined in YAML get grants too. See [Existing compositions after the upgrade](/admin/enterprise-controls/least-privilege-and-trust#existing-compositions-after-the-upgrade).
