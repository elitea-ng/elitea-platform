# Local OAuth protocol fixture

Use this fixture only on an isolated rehearsal network.
It supplies synthetic credentials and read-only marker operations.
It does not use a real provider or a production credential.

## Modes

| Mode | Resource | Credential contract |
| --- | --- | --- |
| `stored` | OpenAPI `GET /stored/api/echo` | Stored OAuth client. Delegated authorization code or client credentials. No DCR. |
| `dcr-public` | Streamable HTTP MCP `/dcr-public/mcp` | DCR public client with PKCE. |
| `dcr-secret` | Streamable HTTP MCP `/dcr-secret/mcp` | DCR-issued client secret with PKCE. |

The stored client identifier is `emulator-stored-client`.
Set `OAUTH_EMULATOR_CLIENT_SECRET` to a separate synthetic secret before starting Compose.
Do not store that value in this repository.

The internal origin is `https://oauth-emulator:8443`.
The browser origin is `https://localhost:18443`.
The only permitted callback is `http://localhost:18084/app/mcp-auth-callback`.
The consent form permits that callback origin in its Content Security Policy.
The fixture validates state, callback, client binding, PKCE, grant reuse, and scopes.
MCP grants also require the exact resource indicator.

Access tokens expire after 60 seconds. Refresh tokens rotate and expire after one hour.
Consent tickets and authorization codes expire after 120 seconds.
Restarting the fixture clears all grants, registrations, and counters.
After restart, authorize again. Existing browser tokens no longer match the fixture state.

## Start

Run these commands from the repository root:

```sh
docker compose -p elitea-oauth-emulator -f deploy/docker-compose.oauth-emulator.yml build
docker compose -p elitea-oauth-emulator -f deploy/docker-compose.oauth-emulator.yml up -d --no-deps
```

The Compose file uses the existing `elitea-rust-rehearsal_default` network.
Set `OAUTH_EMULATOR_NETWORK` to use another isolated network.
Only the loopback interface exposes the browser port.
Do not use this file to recreate Main, the worker, or the rehearsal database.

## TLS trust

The image creates a fixture CA and a server certificate.
The certificate covers `localhost`, `oauth-emulator`, and `127.0.0.1`.
The final image contains no CA private key.

Export `/opt/oauth-emulator/tls/ca.crt` from the fixture container.
Add that public CA to the rehearsal Main and worker trust bundles.
Preserve every existing trust anchor.
Set `SSL_CERT_FILE` to the combined bundle where required.
Restart the affected process after changing its trust bundle.
Do not disable TLS verification in Main or the worker.

`install-worker-trust.py` adds the fixture CA to `/trust/mcp-mock-ca-bundle.pem`.
Run it with the fixture image, a writable rehearsal trust-volume mount, and `OAUTH_EMULATOR_ENABLED=test-only`.
Mount the script into that container. Use its `python3` entrypoint.
The script preserves the first bundle in `mcp-mock-ca-bundle.before-oauth-emulator.pem`.
Repeated installation preserves later trust anchors and does not duplicate this CA.
Review the backup before restoration. Do not remove another operator's later trust changes.

A rebuilt CA requires a new trust installation.
Inspect the certificate before accepting a local browser certificate warning.
Do not change operating-system trust for this isolated proof.

## Browser configuration

Create an OpenAPI toolkit from `/stored/openapi.json`.
Use the stored client identifier, the synthetic secret, and discovery URL `https://oauth-emulator:8443/stored`.
Select `echo_marker` and the scope `records.read offline_access`.
Attach the toolkit to a rehearsal chat, agent, or pipeline.
Do not add DCR fields to the OpenAPI credential contract.

Use separate configured MCP toolkits for the DCR modes.
Those modes test discovery and registration without a paid provider subscription.
Their presence in the fixture does not prove browser-to-worker DCR support.

## Verification

Run the component tests:

```sh
cd deploy/oauth-emulator
python3 -B test_server.py
```

`GET /healthz` reports fixture health.
`GET /stats` reports counts by mode. It returns no token, code, secret, or marker value.
The protected echo result reports the marker, mode, and grant generation.
Use those values with execution identifiers to distinguish authorization, refresh, and resource calls.

Verify consent, two later turns, expiry refresh, regeneration, reload, and toolkit-scoped logout separately.
Verify direct pipeline nodes and parallel branches separately from agent tool loops.
Do not report component tests as browser, restart, load, or production evidence.
