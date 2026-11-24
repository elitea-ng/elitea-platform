# Administration action table (#2582, ADR-0059)

- **What changed:** elitea-main now declares, for every route of the
  administration surface that can change something, the permission the
  action requires, its danger (none, `dangerous` or `critical`), whether it
  asks for a reason, what an elevation may do with it and whether a personal
  access token may call it. A route that takes several actions, or whose
  danger depends on the request, has one entry per action. A test fails when
  a write route of the surface has no entry. The administration surface test
  now also drives the Data retention, Legal holds and agent identity routes,
  which the router mounts only when they are configured.
- **Who is affected:** nobody. The table is declared, not enforced: no route,
  gate, permission, grant or response changed.
- **What to do:** nothing. No configuration, migration or operator action is
  needed. The stories that enforce the table (token refusal, elevation
  restrictions, reasons) each ship their own upgrade note.
