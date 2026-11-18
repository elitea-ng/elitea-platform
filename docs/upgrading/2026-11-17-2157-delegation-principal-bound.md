# Delegation: bound every callee by the run principal (issue 2157, ADR-0044 Amendment 1)

**What changed.** When an agent delegates (a saved agent or pipeline as a tool, a pipeline node, a
sub-agent), the callee can now also be limited to what the person who started the run may use (for
an unattended run, the mandate and the project). The worker command gains an additive field,
`WorkerDelegationPolicyV1.principal_bound`, and the **Configuration > Delegation** section gains the
switch **Bound Delegation By The Run's Principal** (`principal_bound`). The switch is **off by
default**: until you turn it on, nothing changes. No migration.

**Who is affected.** Operators who turn the switch on. While it is on, a composition that reaches
a toolkit its principal may not use (a toolkit the project's folder access hides from that person, or
a credential reference with no active use grant in a governed run) is refused with
`DELEGATION_GRANT_MISSING` and the reason `principal`, in both `enforce` and `report` mode, and the root
agent's own out-of-bounds toolkits are not offered to it.

**What to do.** Roll the release out Main first, then every worker (the chart's default order): a
worker from an earlier release rejects a command that carries the field. Save the **Delegation**
section with the switch on only after every worker runs this release, and after the delegation
migration of epic 2156 has created the grants for your existing compositions. To find affected
compositions first, look at the delegation steps after you turn it on in a test project.
