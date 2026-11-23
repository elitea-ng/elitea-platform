# Administration permissions: the permission registry (#2490, ADR-0059)

- **What changed:** every permission elitea-main resolves in the
  `administration` mode is now named through one code registry
  (`services/elitea-main/internal/adminaccess`). The router, the sub-routers
  and the in-application checks read their permission names from it, and the
  Roles matrix lists every registered name. No permission was added, renamed
  or removed, no route changed and no grant changed. There is no new database
  table: the registry is code.
- **Who is affected:** nobody needs to act. One visible effect is possible: an
  administration permission that no role holds any more (an operator
  unchecked its last holder) is listed again as a row of the Roles matrix, so
  it can be granted again. Before this change such a row disappeared until a
  role held the name again.
- **What to do:** nothing. No configuration, migration or operator action is
  needed, and no behaviour of a gate changes.
