# Internal MCP default-secret policy

## Source mapping

| Current source | New platform owner | Behavior |
| --- | --- | --- |
| Centry `plugins/secrets/api/v2/secrets.py::ProjectAPI.get` | Main `internal/api/v2/secrets/handler.go::List` | Mark configured default names and apply conditional suppression. |
| Centry `plugins/secrets/api/v2/{secrets,secret}.py` mutations | Main shared vault mutation handlers | Refuse protected-name changes when the configured policy suppresses defaults. |
| Core descriptor `default_secret_keys` and `ignore_default_secret_api` | Main `centry.platform_config`, section `default_secrets` | Read current policy through the existing admin configuration service. |
| SDK credential-native `X-SECRET` transport | Main `default_policy.go` | Compare the project vault header without replacing project authorization. |
| SDK internal MCP secret tools | Main `internal_secrets_execute.go`, consumed by Rust native MCP | Reuse the same vault handler and return metadata without secret values. |

Centry paths are under `projects/centry/pylon_main` in the umbrella workspace.

## Implementation history

The existing Main handler always reports `is_default=false` and does not consume the suppression policy.
The shared handler now reads configured names and a suppression flag for each request.
The admin configuration page stores these values through its existing service.
Policy read or validation failures return a safe unavailable response.
The implementation does not store secret values in platform configuration.

Create, update, and delete compare the current vault header inside the existing vault mutation lock.
A refused operation leaves encrypted vault bytes unchanged.
Renaming an ordinary secret onto a protected name also fails.
This closes a legacy rename bypass without removing ordinary secret rotation.
Get preserves the current suppression behavior for ordinary secret values.
Internal MCP has no new header or secret-value argument.
The shared Main router supplies this policy to both REST and MCP.

## Verification

- The full vault suite passes 113 cases with zero skips.
- Admin policy persistence and permission-declaration tests pass.
- Invalid admin policy writes preserve the previous stored values.
- The combined draft and secret selection passes 24 cases with zero skips.
- The PostgreSQL fixture verifies names, metadata, header suppression, denied writes, and ordinary rotation.
- A matching header without authentication still receives HTTP 401.
- The MCP fixture verifies hidden default names and byte-identical encrypted storage after refusal.
- API bindings are regenerated from the corrected OpenAPI metadata description.

These tests do not prove deployed Rust chat behavior or production activation.
