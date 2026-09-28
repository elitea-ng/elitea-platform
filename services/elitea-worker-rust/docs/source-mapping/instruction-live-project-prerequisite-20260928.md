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

## Rehearsal follow-up

Main revision `723ac75d5` is deployed from a clean archive.
Image: `elitea-main:project-groups-723ac75d5`.
Digest: `sha256:e6bc79fb6840ec8f6c3109f646f8527f54a49c066a2ff59c55b8df2f5e54f43c`.
The deployment retains all six mounts, environment values, networks, and resource limits.
The corrected project-model step passes against the restored database.

The next step exposes a missing `public.create_tenant_schema(text)` function, with PostgreSQL error `42883`.
The restoration installs only the function definition from the same committed bootstrap file.
It checks that the function is absent before installation.
It does not execute the bootstrap seed, existing-project DDL, or existing-project migrations.
The function source SHA-256 is `e510832801b6d8632ded255f248d0055f35a7063bacff1c9b9751849ae56ac0f`.
Local procedure: `/private/tmp/elitea-install-tenant-helper.py`.

The next request passes project model, tenant schema, permissions, system identity, project secrets, and artifact buckets.
It fails at `project_pgvector` with `project pgvector provisioning unavailable`.
The underlying vector-store cause is not yet established.
The test does not disable this check or modify the public vector-store configuration.

Readback confirms only the original projects 1 and 2 remain.
Failed attempts 116 and 117 leave no tenant schemas.
This readback does not establish cleanup of external vector-store resources.
No skill, application, or chat fixture is created.
Private and Public project contexts remain unchanged.

The remaining isolated-project prerequisite is vector-store provisioning.
The planned live compaction verification remains open.
