# Admin console: the index is Home (#2533, ADR-0059 R3.3a)

**What changed.** With the six labelled areas on
(`admin_console.admin_console_areas`), `/admin/app/` is **Home** instead of the
Users page:

- one card per area the caller's roles open, with the items it opens there;
- the queues the caller decides, each with its count: App requests, Connector
  requests and Delegations awaiting review. A queue the caller can only view
  is not shown.

A caller whose roles open exactly one area is redirected to that area's first
item. A caller whose roles open no area sees a no-access page with the
deployment's IT support contact and no navigation.

The counts come from the queues' existing list routes; no API route was added.
With the areas off, `/admin/app/` still renders Users.

**Who is affected.** Administrators who bookmarked `/admin/app/` to reach
Users, and runbooks or scripts that expect Users at the index.

**What to do.** Use `/admin/app/users` for the Users page.
