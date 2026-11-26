# Break-glass: what an elevated person gives is provisional (#2526, #2606, #2604, ADR-0059)

**What changed.** A role or a project membership that a person under a
break-glass elevation gives to SOMEONE ELSE is no longer a standing
assignment. It is a provisional grant, bound to the elevation:

- An administration role given on the Users page (`set_admin_role`) or on
  **Modes**, and a project membership given by a bulk invite, or by an
  invite or a role edit in a project the elevated person is not a member
  of, are recorded in the new table `elitea_auth.admin_provisional_grants`
  (shared migration 0284), not in `auth_core__user_role` or
  `auth_core__project_user_role`. The responses carry `provisional: true`.
- The grant is in effect only while that elevation is active. It stops at
  once when the elevation ends or expires.
- The elevation's review confirms it: a `justified` review names the grants
  to keep in `confirm_grant_ids`, and each becomes a standing assignment
  written by the reviewer. An `unauthorized` review confirms nothing.

Shared migration 0285 adds the operator's notes and the acknowledgement of
an elevation nobody independent could review to
`elitea_auth.admin_elevation_items`, with the new command
`elitea-main break-glass note` and the route
`POST /api/v2/admin/access/elevations/{elevationID}/acknowledge`.

**Who is affected.** Only people acting under an elevation, including a
standing global administrator's elevated session. Standing role and
membership changes are unchanged. Nothing existing is rewritten: both
migrations only add.

**What to do.** Nothing for the upgrade. After an incident, review each
elevation and confirm the grants that should stay; until then the people
they were given to do not hold them. See
[Break-glass access](../../apps/elitea-docs/src/content/docs/admin/break-glass.mdx).
