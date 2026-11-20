---
date: 2026-11-18
order: 8
---
## Toolkit indexes, internal storage, shared links and SharePoint sign-in

- **List indexes** now names every index the toolkit has on its **Indexes** tab, not only those that already stored vectors. An index that was created and never ran, or whose first run failed, is listed too; searching it answers that it holds no searchable content yet instead of "not found". Before, the tool said "No indexed collections" next to a tab full of indexes. Upgrade the worker before Main.
- The platform's own `index-artifacts` bucket (index results and toolkit discovery files) is no longer listed under **Artifacts** and its address answers "not found" to everyone. Nothing in it was deleted or moved, so existing deployments need no action.
- **Copy link** on a toolkit, and **Share** on an agent or pipeline, now copy an address that names the project. Opening it from another project switches to the project the link was copied from, then opens the entity. Before, it opened in whichever project you had selected.
- Signing in to SharePoint (and other Microsoft OAuth toolkits) no longer fails with `AADSTS650053` when the credential lists its scopes with commas ("User.Read, Chat.Read"): scopes are sent space-separated on both the sign-in and the token request.
