# Delegation grants: the attach review, and per-toolkit approver access (issue 2159)

**What changed.** Choosing an agent or pipeline in the Tools panel opens a review of what it will
be able to use, and **Approve and attach** attaches it and approves its delegation grant in one
step. The web and desktop apps show it, with the grant state on each attached agent's row; the
mobile app lists an agent's delegations (client contract 1.59: five delegation operations are now
`client`-tagged, and the preview answers `governed`). The approver access check, which was the two
project permissions, is now per toolkit (the project's folder access rules can hide a toolkit from
the approver) and, under the governed profile, per credential reference (an active credential use
grant must cover it).

**Who is affected.** Editors who attach agents. An approver who held the two permissions but is
hidden a toolkit by folder access, or (governed profile) a credential no use grant covers, now
approves less than before: the rest is recorded as omitted. Nothing that runs today changes, and no
setting is added.

**What to do.** Upgrade Main and the web app together (the client contract is 1.59). Nothing else.
Pending grants that attaches created before this release can be approved from **Review delegation**
on the row. The switch `ELITEA_DELEGATION_GRANTS_ENFORCED` is unchanged and stays `"false"` until
the migration of existing compositions lands.
