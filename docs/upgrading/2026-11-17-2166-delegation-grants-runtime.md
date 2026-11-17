# Delegation grants: the worker reads approved grants, behind a switch (issue 2166)

**What changed.** The main service can send the approved delegation grants of a run to the worker,
and the worker bounds each delegation by `own(callee) ∩ (E(caller) ∪ grant)`: a callee may use what
its caller holds and what the edge's approved grant lists, and what it holds beyond that is narrowed
out and recorded on the delegation step (`delegation_grant_id`, `delegation_grant_digest`,
`delegation_narrowed`). The grant covers one edge only. New proto fields on the command's delegation
policy (grants, root application id, grants-enforced flag) and one new setting:
`main.env.ELITEA_DELEGATION_GRANTS_ENFORCED` (`"true"` or `"false"`, default `"false"`).

**Who is affected.** Nobody, until the switch is turned on. With it off (the default) commands are
unchanged byte for byte and delegation behaves exactly as before.

**What to do.** Upgrade the main service first and the workers after it (the chart's default order).
Leave `ELITEA_DELEGATION_GRANTS_ENFORCED` at `"false"` until the migration of existing compositions
has created their pending grants and the owners of the calling agents have approved or rejected them;
then set it to `"true"` and restart the main service. Every worker must already run this release:
a worker from an earlier release refuses a command that carries grants. To roll back, set it to
`"false"`. No agent's reach is widened by the upgrade: a grant can only let a callee use what the
approver held access to, and never more than the person or mandate running it could use.
