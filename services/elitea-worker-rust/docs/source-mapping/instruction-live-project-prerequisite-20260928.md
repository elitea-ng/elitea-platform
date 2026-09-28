# Isolated instruction verification prerequisite

The live instruction-revision test needs a dedicated project.
Changing shared project context would affect unrelated chats.

The first project-create request fails before skill or chat creation.
Main logs PostgreSQL error `23502`: `centry.project.keycloak_groups` rejects a null value.
The restored database has a non-null JSON column without a server default.
The API reports successful compensation. A readback shows only the original two projects.

## Source mapping

| Current-platform source | Behavior | Replatform owner |
| --- | --- | --- |
| `projects/centry/pylon_main/plugins/projects/models/project.py::Project.keycloak_groups` | SQLAlchemy supplies an empty dictionary as a client-side default. | `services/elitea-main/internal/application/projectprovisioning/steps.go::createProjectModel` |
| Replatform bootstrap `internal/infra/db/migrations/001_initial.sql` | A fresh schema supplies an empty JSON object as a server default. | Existing bootstrap remains unchanged. |

The project insert now supplies an empty group mapping explicitly.
It works with fresh schemas and restored current-platform schemas.
No schema, migration, credential, authorization, or worker behavior changes.

## Verification

`TestProvisionBuildsATenantEqualToTheReference` removes the server default in its isolated test database.
It provisions the full tenant and reads back the empty group mapping.
The PostgreSQL test and package `go vet` pass.
The product database is not used as the test database.

Rehearsal deployment and isolated project creation remain pending at this commit.
This prerequisite does not close the live instruction-revision compaction test.
