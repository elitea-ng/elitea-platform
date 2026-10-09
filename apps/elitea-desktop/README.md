# elitea-desktop

The Elitea desktop client (ADR-0029, decision 9; sign-in per ADR-0025): a Tauri 2
host around the `desktop` build mode of `apps/elitea-web`. This is the client
shell. The local agent runtime, local tools and sandbox come in later phases.

```
apps/elitea-web  --vite build --mode desktop-->  apps/elitea-web/dist-desktop
                                                        |  bundled as assets
apps/elitea-desktop/src-tauri (Rust host) <--IPC--> the bundled webview
        |   keychain (refresh token)        |
        |   loopback sign-in listener       +-- HTTP plugin --> your deployment
        +-- system browser (RFC 8252)
```

## What the host does

- **Connect**: you enter a deployment address; the host fetches
  `/.well-known/elitea-client`, shows the brand display name, and refuses
  `http://` except for `localhost` / `127.0.0.1` / `[::1]`. Every URL in the
  discovery document must be on the address you typed.
- **Sign in**: authorization code + PKCE S256 in the **system browser**, with a
  one-shot loopback redirect `http://127.0.0.1:<ephemeral port>/callback`. The
  `state` and the RFC 9207 `iss` are checked before the code is exchanged.
- **Tokens**: the refresh token lives in the OS keychain only (service
  `ai.elitea.desktop`). The webview is handed a 15-minute access token over IPC
  and holds it in memory; it never reaches web storage. Refresh rotates the
  token; a lost response is retried with the old token (the server re-delivers
  within its grace window). `device_revoked` wipes local state. Sign-out
  revokes the device session server-side (falling back to the revocation
  endpoint stored with the token when discovery is unreachable); a revoke that
  does not get through is kept in a second keychain item (`pending-revoke`)
  and retried at the next launch, while the local session is forgotten at once.
- **Client policy**: stored verbatim (`client-policy.json` in the app config
  directory) from every token response. Enforcing `local_work` comes with the
  local runtime.
- Every request carries `X-Client-Version` (the deployment's 426 gate).

## Registering the client

The deployment must list this client in its `native_clients` configuration:
client id, display name and the loopback redirect (`http://127.0.0.1` with any
port). The default client id is `desktop`. To use another one, in order of
precedence: the `ELITEA_DESKTOP_CLIENT_ID` environment variable at run time,
`client_id` in `settings.json` in the app config directory, or
`ELITEA_DESKTOP_CLIENT_ID` at build time.

## Security model

- The window loads **only bundled assets**. `on_navigation` refuses every other
  origin, so remote content never sits next to the IPC commands.
- IPC is an explicit allowlist: eight `host_*` commands plus the local-work
  commands (workspaces and the D0 local agent turn), granted by
  `capabilities/default.json` to the `main` window only (no remote origin is
  listed). No command returns the refresh token. The whole surface, with the
  `agent://event` stream, is in `src-tauri/IPC.md`.
- Local agent turns (`src-tauri/src/d0/`, the temporary D0 assembler) call
  the connected deployment only, with the native access token: the resolved
  definition, the local turn start/commit, the remote toolkit call and
  `/llm`. No toolkit secret reaches the device; an agent whose definition
  withholds secrets is refused locally.
- Images and media load from `self`, `data:` and `blob:` only (no remote
  `https:`). The deployment is chosen at run time, so a static CSP cannot name
  it; and the webview sends no cookies, so an `<img>` pointed straight at the
  deployment could not authenticate anyway: authenticated images (chat
  attachments, artifacts) are fetched with the bearer and shown as `blob:`
  URLs. The cost is that third-party images (an identity provider's avatar
  URL, an image link inside Markdown) do not render and fall back to
  initials / alt text. In exchange, model-written Markdown cannot use an image
  URL to send conversation data to another host.
- The opener hands only `https` and loopback `http` URLs to the system browser.
- The CSP allows scripts from `self` only. The webview reaches the deployment
  through the HTTP plugin (a Rust-side request, so no CORS change is needed on
  the deployment). `capabilities/default.json` grants that plugin no URL at
  all; `src-tauri/src/http_scope.rs` adds a runtime capability scoped to the
  connected deployment's origin (at launch for the stored one, and on every
  connect). Runtime capabilities cannot be removed, so scopes accumulate per
  process: an origin connected earlier stays reachable until the app
  restarts. The bearer token is still attached only to the current origin
  (checked in the webview's HTTP core and SSE client).

## Develop

```sh
npm ci                      # the Tauri CLI
npm --prefix ../elitea-web ci
npm run dev                 # vite dev server + the host
npm run build:debug         # a debug .app bundle
```

Rust checks (`rust-toolchain.toml` pins the toolchain):

```sh
cd src-tauri
cargo fmt --check && cargo clippy --locked --all-targets -- -D warnings && cargo test --locked
```

`tauri build` runs `npm run build:desktop` in `apps/elitea-web` first.
Icons are the brand mark rasterised with
`npx tauri icon ../elitea-web/public/brand/logo-mark.svg -o src-tauri/icons --fit contain`
(keep the six files `tauri.conf.json` lists).

## Release signing (stubs, no secrets in the repository)

macOS first. A release build is signed with the hardened runtime and notarised
when these are set in the build environment (see the Tauri macOS signing guide):

| Variable | Purpose |
| --- | --- |
| `APPLE_SIGNING_IDENTITY` | `Developer ID Application: ...` identity in the build keychain |
| `APPLE_CERTIFICATE`, `APPLE_CERTIFICATE_PASSWORD` | base64 `.p12` to import in CI |
| `APPLE_ID`, `APPLE_PASSWORD`, `APPLE_TEAM_ID` | notarisation with an app-specific password |
| `APPLE_API_ISSUER`, `APPLE_API_KEY`, `APPLE_API_KEY_PATH` | notarisation with an App Store Connect API key instead |
| `TAURI_SIGNING_PRIVATE_KEY`, `TAURI_SIGNING_PRIVATE_KEY_PASSWORD` | signs updater artifacts (the updater is not enabled yet) |

Unsigned local builds work without any of them. Telemetry is off; there is none.
