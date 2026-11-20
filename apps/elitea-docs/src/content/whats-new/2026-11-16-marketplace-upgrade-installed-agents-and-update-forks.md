---
date: 2026-11-16
order: 2
---
## Marketplace: upgrade installed agents and update forks

An installed agent gets **Upgrade to...**, which shows the difference, the release notes, the rating and what a version adds before you choose any version, up, down or across. At low tier it is one click, and above it a change request of kind `upgrade` waits for an approver. An **Upgrade policy** (pinned, notify, auto) says whether the platform may move the agent by itself. Auto works only at low tier and never takes a version that adds a tool, a capability or a requirement, or raises the tier. A fork shows **Upstream update available** and a three-way view (base, theirs, ours) with conflicts first; the merge is a new draft and a change request of kind `upstream_merge`, never applied without a person. A yanked version raises a banner and a notification for every project that holds it, and an administrator can also suspend those agents. **Run the migrations** (shared `0251`) and upgrade Main and the web app together. See [Marketplace](/menus/marketplace#upgrade-an-installed-agent).
