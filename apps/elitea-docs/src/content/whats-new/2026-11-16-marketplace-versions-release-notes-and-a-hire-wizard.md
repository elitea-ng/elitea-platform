---
date: 2026-11-16
order: 1
---
## Marketplace: versions, release notes and a hire wizard

Every published agent and skill is now a **listing** with versions. Each version keeps a snapshot, release notes, a digest, and its requirements (toolkit and MCP types, skills, nested agents, model class, tier; never credentials). A publisher can deprecate or yank a version. An administrator can yank, delist, or feature a listing. The Browse sidebar searches by kind, category, tag, publisher, tier, required types, rating, and hired here. **Hire** opens a four-step wizard (version, mode, bindings, review). **Install** makes a read-only agent pinned to the version, and **Fork** makes an editable agent that records its origin. A hire above low tier, or with a write tool, starts with tools disabled and a change request of kind `hire`. The Workshop gains the **Installed** origin and an **Update available** filter. Unpublish now delists and keeps the snapshot while a project holds it. **Run the migrations** (shared `0250`) and upgrade Main and the web app together. See [Marketplace](/menus/marketplace).
