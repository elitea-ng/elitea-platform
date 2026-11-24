# Configurations: the mode-less project configuration routes take the central settings branch (#3024)

**What changed.** With `ELITEA_CONFIGURATIONS_ENABLED=true`, `elitea-main`
serves `GET /api/v2/configurations/configurations/{projectID}` and
`GET /api/v2/configurations/configuration/{projectID}/{configID}` from its
reviewed read routes, and with `ELITEA_CONFIGURATIONS_MUTATION_ENABLED=true`
also the `POST`, `PUT` and `DELETE` on the same paths. Those routes take
precedence over the compatibility routes at the same paths. Until now they
lacked two rules that the compatibility routes have:

- **The central settings branch (ADR-0059 R0.6b).** A holder of
  `projects.settings.view` reads, and a holder of `projects.settings.manage`
  writes, the five guarded project settings (`cloud_executor`,
  `worker_egress`, `ai_disclosure`, `browser`, `computer_use`) of a project
  they are not a member of. Before this change, only the `{mode}/{projectID}`
  paths gave such a caller this access. Credential, model and toolkit
  configuration types stay refused to a non-member. A central operator reads
  every row of a guarded type, including a member's unshared row, as on the
  compatibility routes. It may also change or delete those rows.
- **The guarded-type rule.** A project member writes a guarded setting only
  when they hold `models.project_settings.edit` in the project, or hold
  `projects.settings.manage` centrally. The reviewed write routes did not apply
  this rule. A member who could create configurations could therefore write a
  guarded setting on an install with the mutation flag on. That flag is off in
  every shipped values file and compose file.

The routes are also marked as administration routes, so the maintenance
window treats them the same way it treats the compatibility routes.

**Who is affected.** Central operators who manage project settings, on installs
with `ELITEA_CONFIGURATIONS_ENABLED=true`: they can now use the mode-less paths
that the web client calls. Project members without `models.project_settings.edit`
on installs with the mutation flag on: their writes of a guarded setting now
answer `403`.

**What to do.** No operator action.
