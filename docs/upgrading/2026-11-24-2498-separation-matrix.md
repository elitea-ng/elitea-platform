# Administration: the separation matrix and the surface router audit (#2498, #2985)

**What changed.** Two tests now cover the administration surface of
`elitea-main`. Neither changes what the server does.

- **The separation matrix (#2498, ADR-0059 R0.9a).**
  `services/elitea-main/internal/api/testdata/separation_matrix.txt` lists
  every route and action of the administration surface. For each built-in role
  (`super_admin`, `admin`, `llm_manager`, `project_administrator`,
  `security_auditor`, `emergency_access`), it says whether the server admits
  the request with that role's standing permissions. CI fails when the server
  answers differently. Use the file as the reference for what each role can
  reach. The role write paths are marked `pending #2587`, and a later release
  pins them.
- **The surface router audit (#2985).** A test in `cmd/elitea-main` fails when
  `main.go` composes a route group that the administration surface tests do not
  drive, unless the test records why that is safe. The composed test router now
  also mounts the credential grant, probe and consent routes, the kill switch
  handler and a gateway status reader.

**Who is affected.** Developers who add or change an administration route: they
regenerate the matrix and review the lines that changed (see the backend
developer guide). Operators are not affected.

**What to do.** No operator action.
