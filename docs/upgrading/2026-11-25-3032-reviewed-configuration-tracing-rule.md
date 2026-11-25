# Configurations: every project configuration route applies the tracing rule (#3032)

**What changed.** A configuration of a tracing type (Langfuse, or any type
whose registry schema names the `tracing` category) is managed only by a
caller who manages tracing in the project: a project administrator, or the
owner inside their own personal project. The compatibility routes applied this
rule to the create, the update and the read by id. Three routes did not:

- **The compatibility delete.** `DELETE
  /api/v2/configurations/configuration/{projectID}/{configID}` (and its
  `{mode}` form) deleted a tracing configuration for any member who held
  `configurations.configuration.delete`. It now answers `403` with
  `tracing configurations are managed by project admins`, like the update.
- **The reviewed read by id.** With `ELITEA_CONFIGURATIONS_ENABLED=true`,
  `GET /api/v2/configurations/configuration/{projectID}/{configID}` is served
  by the reviewed read route, which returned a tracing configuration to any
  member. It now answers `404`, as the compatibility route does.
- **The reviewed writes.** With `ELITEA_CONFIGURATIONS_MUTATION_ENABLED=true`,
  the reviewed `POST`, `PUT` and `DELETE` routes now refuse a tracing write
  with the same `403`: the create on the body's type, the update and the
  delete on the stored row's type. That flag is off in every shipped values
  file and compose file, so this part changes nothing until it is turned on.

**Who is affected.** Project members who are not project administrators and
who deleted, or read by id, a project's Langfuse configuration. The
available-types catalogue already hid tracing types from them, so they could
not create one.

**What to do.** No operator action.
