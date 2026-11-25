# What an elevation cannot do: `403 elevation_restricted` (#2522)

**What changed.**

- Every administration action carries an elevation value in the action
  table (`allow`, `flag`, `deny`, `sealed`). It is now enforced in front of
  every route gate, for a browser session or sealed session whose permission
  for the action came from an active break-glass elevation:
  - `deny` (the break-glass section, the email section, the permission
    matrix, user project permissions, role create and delete, SCIM group
    bindings, legal hold release, retention shortening): `403
    elevation_restricted` at either level;
  - `sealed` (identity provider trust, delete and rebind, SCIM clients):
    `403 elevation_restricted`, except under a sealed elevation at the
    `super_admin` level, where the action is allowed and flagged;
  - `flag`: allowed, and the admin audit row is marked
    `elevation_flagged = true` for the elevation review.
- A person whose standing roles already admit the action is not restricted.
- The identity trust gate opens to a sealed `super_admin` elevation, read
  from the sealed credential's full resolution; it stays closed to every
  approved elevation.
- **Create token** from an elevated browser session, or from any sealed
  session, answers `403 elevation_restricted`. A request with admin scopes is
  refused first by the scope rules, with their own codes
  (`token_scopes_elevated`, `token_scopes_sealed_session`).
- Shared migration **0273** adds the column `centry.audit_events.elevation_flagged`
  (boolean, nullable, outside the chain hash; existing rows stay null).

**Who is affected.** No route starts an elevation yet, so on a deployment
without elevations nothing changes. A client that reads `403` codes should
treat `elevation_restricted` as final for the elevation: the action needs a
standing role.

**What to do.** Nothing. Run the migrations as usual; 0273 is an
`ADD COLUMN IF NOT EXISTS` and takes no table rewrite.

See [Break-glass access: what an elevation cannot do](../../apps/elitea-docs/src/content/docs/admin/break-glass.mdx).
