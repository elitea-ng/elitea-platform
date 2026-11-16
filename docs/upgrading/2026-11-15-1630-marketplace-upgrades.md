# Marketplace: upgrades and upstream updates (ADR-0051 M4, issue 1630)

**What changed.** A new shared migration adds the upgrade policy of an install
(`elitea_marketplace.installs.upgrade_policy`, default `pinned`) and the payload
of a Marketplace change request (`elitea_lifecycle.change_requests.marketplace_payload`).
The number is `0251` in the pull request and is renumbered above the head of
`main` at merge. A new notification type set (`marketplace_version_yanked`,
`marketplace_update_available`, `marketplace_upgrade_needs_approval`,
`marketplace_upgrade_applied`) is written to `centry.notifications`.

**Who is affected.** Every operator. Existing installs keep the `pinned` policy,
so nothing upgrades by itself until a person sets `notify` or `auto`.

**What to do.** Run the migrations, then upgrade Main and the web app together.
No other action is required.

A platform administrator who yanks a version can ask to suspend the agents
installed from it. The suspension is an agent-scope suspension in the kill
switch with a reason that starts with `listing_yanked`; resume it there. A
restore of the version does not resume it.
