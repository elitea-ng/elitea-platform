# Marketplace: listings, versions and hires (ADR-0051 M3, issue 1629)

**What changed.** A new shared migration adds the schema `elitea_marketplace`
(listings, listing versions, installs) and widens `elitea_lifecycle.change_requests`
with the kinds `hire`, `upgrade` and `upstream_merge`. The number is `0250` in the
pull request and is renumbered above the head of `main` at merge. Publish and
unpublish now write the Marketplace records in their own transaction. Unpublish
delists and keeps the snapshot while a project has it installed.

**Who is affected.** Every operator. Versions published before this release get
their listings built on the first read of the Marketplace. The service reads the
public project's published versions and writes the records once.

**What to do.** Run the migrations, then upgrade Main and the web app together.
No other action is required. Optional: a platform administrator can switch on the
Agent Lifecycle policy `marketplace_personal_self_approve` (off by default) so a
person can approve their own Marketplace hire in a personal project.

A new routine, `main.marketplace_retention`, runs once a day. It deletes withdrawn
Marketplace snapshots that no project has installed or as a fork origin, 30 days
after the withdrawal. The 30 days are the platform's choice, not a number from the ADR. It pauses in maintenance. Find it in the routines list.
