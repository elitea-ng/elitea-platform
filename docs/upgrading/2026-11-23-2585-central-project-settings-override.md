# Guarded project settings of any project through a central override (#2585)

**What changed.** The configuration routes of a project admit the central
permissions `projects.settings.view` (read) and `projects.settings.manage`
(read and change) for the five guarded project settings of any project,
without a membership (ADR-0059 R0.6b):

- `cloud_executor`, `worker_egress`, `ai_disclosure`, `browser` and
  `computer_use`.
- On `GET /configurations/configurations/{projectID}?type=…` (the `type`
  filter is required and every listed type must be guarded),
  `GET`/`PUT`/`DELETE /configurations/configuration/{projectID}/{configID}`
  (decided on the stored row's type, and for `PUT` on the requested type as
  well) and `POST /configurations/configurations/{projectID}` (decided on the
  body's `type`), and on their `{mode}` twins.

The allowed (route, configuration type) pairs are an allowlist in
`internal/adminaccess/central_override.go`, reviewed line by line in
`internal/adminaccess/testdata/central_override.reviewed`; a test fails when
the two differ. Credential, model and toolkit configurations are on no pair
and stay refused. Project members with `models.project_settings.edit` keep
the existing path unchanged.

**This is a privilege increase.** The built-in `super_admin`, `admin` and
`project_administrator` roles hold both names (since R0.2 and R0.7), so after
this upgrade they can read and change these five settings in every project.
Each such use marks its audit record `central_override` (the storage column
arrives with ADR-0059 R1.4a).

**Who is affected.** Deployments whose administrators should not change a
project's cloud executor, egress, disclosure, browser or computer-use
settings.

**What to do.** If that is your case, give those people a custom
administration role without `projects.settings.manage` (and, for reading,
without `projects.settings.view`) instead of the built-in ones.
