# Administration surface: found by driving the router (#2581, ADR-0059)

- **What changed:** a test now finds the administration surface of
  elitea-main (every route whose decision resolves the `administration` mode)
  by sending a request to every mounted route of the composed router, and
  fails when one of those checks names a permission outside the permission
  registry, or when a route under an administration prefix could not be
  reached. `RouterConfig` gains one test-only field, `PermissionResolver`,
  which overrides the database resolver; production leaves it unset, and a
  test fails if the binary links the test resolver.
- **Who is affected:** nobody. No route, gate, permission, grant or response
  changed.
- **What to do:** nothing. No configuration, migration or operator action is
  needed.
