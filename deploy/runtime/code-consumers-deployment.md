# Code consumer deployment

Main owns repository acquisition, scoped platform calls, and authorized debug artifact publication.
Worker owns graph checkpoints. Supervisor owns isolated execution resources.
Runner receives selected state and signed, bounded platform authority.

These consumers are on by default wherever the agent runtime runs (PLAN §8.3): the standalone compose stack and the
Helm chart with the runtime enabled. Deployment material stays configurable and is required.
Enabling a consumer does not grant repository, credential, toolkit, or artifact access.
Main applies the current actor, project, resource, and execution permissions.

## Required deployment state

Apply the release's shared and AgentState migrations through their owning migration commands.
Configure Agent dispatch, original Code owner recovery, and exact Supervisor audiences.
Configure native object storage and current credential and toolkit resolution.
Use the original AgentState database with current fenced checkpoint writer rows.
Keep HTTP and deferred graph admission disabled unless their separate acceptance gates pass.

## Docker Compose

`docker-compose.standalone-full.yml` enables the owner recovery, workspace, platform, and debug consumers itself;
no overlay is needed. It introduces no additional public port.

`deploy/scripts/standalone-stack.sh certs` runs `deploy/scripts/code-material.py certs`, which issues Main's Code owner
client certificate (`dns:elitea-code-owner-main`, client authentication, signed by the runtime CA) and the private
platform content key into `deploy/certs/runtime/`. `runtime-material` installs them from there. The consumer
configurations (owner identity and the three execution Supervisor origins, workspace policy, broker policy) are fixed
values in the compose file.

Installed material references use the fixed paths under `/run/elitea-runtime/`.
The debug DSN uses `/run/elitea-runtime/agent-checkpoint-connection`.
The material installer copies private owner and broker files only into Main's volume.
Private files use mode `0600`. The public owner certificate uses mode `0644`.
The installer rejects missing, symbolic-link, empty, and oversized Code material before copying.
Main validates the document contents through its existing secure readers.

## Helm

Configure the typed `main.runtime` blocks in `values.yaml`:

- `codeOwnerRecovery`: Main identity and exact Supervisor audience/HTTPS origin pairs.
- `codeWorkspace`: repository capabilities, exact host:port egress entries, and bounded policy.
- `codePlatform`: bounded broker policies.
- `codeDebugArtifacts`: explicit enablement.

Put the owner TLS, private broker document, and debug DSN files in the configured Main runtime Secret.
The existing non-root runtime material init container copies and verifies these files.
Worker and Runner receive no private Main content keys.
Runner requires a separate public Main receipt verification asset in its immutable runtime image.
Use the same pinned Runner image selections in Docker and Kubernetes execution profiles.

The chart rejects unknown settings, inactive dependencies, duplicate policies, wildcard egress, and invalid numeric bounds.
Policy limits remain JSON numbers. The chart rejects quoted, fractional, zero, and excessive limits.
Both deployment paths use the same runtime environment names and JSON contracts.
The chart adds four AgentState receipt connections per Main replica to its database budget.
Debug and compiled receipts share this pool when their canonical DSN file paths match.
The chart uses the same DSN path and counts the shared pool once.
Default-disabled consumers add no receipt pool.

## Trust boundaries

The private Main platform content key document encrypts authorized platform content.
The public Runner receipt asset verifies signed Main replies.
These documents have different schemas and purposes.
Never copy private content keys, certificate keys, or operator credentials into a Runner image.
Never expose unrestricted storage credentials or a Docker socket to executable code.

## Verification and limits

Run `python3 scripts/runtime/test_code_consumers_deployment.py` for rendered Helm and Compose parity checks.
Run `python3 scripts/runtime/test_code_material_install.py` for synthetic material copy and mode checks.
Run the existing compiled snapshot packaging tests to verify receipt pool compatibility.
Run `python3 -B scripts/runtime/test_code_platform_public_trust.py` for public receipt trust packaging checks.
The Taskfile and Helm CI workflow include the new checks.

These checks render configuration and use synthetic material.
They do not prove real certificate identities, live database authority, container execution, or browser acceptance.
The exact assembled Docker and Kubernetes cohorts still require runtime verification.
See the [combined source mapping](../../services/elitea-worker-rust/docs/source-mapping/code-root-composition-20261005.md).
