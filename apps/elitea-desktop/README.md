# elitea-desktop

The Elitea desktop client (ADR-0029, decision 9; sign-in per ADR-0025): a Tauri 2
host around the `desktop` build mode of `apps/elitea-web`. This is the client
shell. The local agent runtime, local tools and sandbox come in later phases.

```
apps/elitea-web  --vite build --mode desktop-->  apps/elitea-web/dist-desktop
                                                        |  bundled as assets
apps/elitea-desktop/src-tauri (Rust host) <--IPC--> the bundled webview
        |   credentials.json (refresh token)|
        |   loopback sign-in listener       +-- one pooled HTTPS client --> your deployment
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
- **Tokens**: the refresh token lives in one owner-only file only,
  `credentials.json` in the app config directory (see "Security model"),
  read once per launch and then kept in memory. The webview is handed a 15-minute access token over IPC
  and holds it in memory; it never reaches web storage. Refresh rotates the
  token; a lost response is retried with the old token (the server re-delivers
  within its grace window). `device_revoked` wipes local state. Sign-out
  forgets the local session at once and queues the server revoke in a second
  slot of the same file (`pending-revoke`) before any network call; the revoke
  is sent in the background (falling back to the revocation endpoint stored
  with the token when discovery is unreachable) and leaves the queue once the
  server confirms it, otherwise it is retried at the next launch. No lock is
  held across the network, so nothing waits on a slow deployment.
- **Client policy**: stored verbatim (`client-policy.json` in the app config
  directory) from every token response. Enforcing `local_work` comes with the
  local runtime.
- Every request carries `X-Client-Version` (the deployment's 426 gate).

## Native features

macOS first; Linux and Windows build and degrade as noted.

- **Window chrome (macOS)**: no title bar of its own (`TitleBarStyle::Overlay`,
  hidden title); the traffic lights sit inside the sidebar's top area
  (`trafficLightPosition` 18, 22). The window is transparent over the
  system sidebar material (`NSVisualEffectMaterial::Sidebar`, following the
  window's active state): Tauri's own `effects` API, which applies it with
  the `window-vibrancy` crate, so no direct dependency. The web shell asks
  `app_platform` and then draws a `data-tauri-drag-region` and leaves
  `traffic_light_inset_px` free on the left; content painted opaque simply
  hides the material. **Trade-off**: a transparent WKWebView needs the
  private `drawsBackground` key. Tauri 2.12.1 always enables it (the old
  `macos-private-api` feature / `macOSPrivateApi` flag is a no-op now), so
  nothing is configured for it, but it keeps the app off the Mac App Store
  (Developer ID distribution is unaffected). Linux and Windows keep normal
  decorations and an opaque window.
- **Window state**: size, position, maximized and full screen persist
  (`tauri-plugin-window-state`, Rust-side, no webview permission); the
  minimum size is 900 × 600. On macOS closing the window (⌘W, the red
  button) hides it; the dock icon brings it back and ⌘Q quits, so a
  running turn keeps going and keeps notifying.
- **Menu bar and shortcuts** (`src/menu.rs`): App (About, Settings… ⌘,,
  Services, Hide, Quit), File (New Thread ⌘N, Open Folder… ⌘O, Close Window
  ⌘W), Edit (the OS's own undo/redo/cut/copy/paste/select all, so they keep
  working in every text field), View (Command Palette ⌘K, Toggle Sidebar
  ⌘\\, Toggle Changes Panel ⌘⌥\\, Back ⌘[, Forward ⌘], Reload ⌘R in debug
  builds only, Actual Size ⌘0, Zoom In ⌘=, Zoom Out ⌘-, Full Screen), Window,
  Help (Elitea Help opens `<deployment>/docs/` when connected). On Linux and
  Windows Settings and Quit are in File and About in Help; shortcuts use
  Ctrl. UI actions arrive as `app://command` (IPC.md).
- **Opening folders**: Open Folder… runs the native picker host-side;
  a folder dropped on the window, or on the dock icon / opened with Elitea
  from Finder (`Info.plist` declares folders as an *Alternate* document
  type, so Elitea never becomes the default for folders), is opened through
  the same `WorkspaceStore::add` as `workspace_open`, then the UI gets
  `workspace_opened`. Dropped files (not folders) are handed to the UI as
  `files_dropped` with their absolute paths; the host does nothing else
  with them. A drop on the window is handled natively, so the webview gets
  no HTML5 file drop events.
- **Notifications and dock badge** (`src/attention.rs`): while the window is
  not focused (or hidden), an `approval_request` posts "Approval needed —
  <workspace>" naming only the tool, and a turn's `done` posts "Turn
  finished / failed / stopped — <workspace>". Never a command line, a path,
  file contents or a secret. The dock badge (macOS; Linux where the desktop
  supports it; Windows has none) counts approvals still open across
  workspaces, cleared as they are answered or their turn ends. Permission
  is asked lazily on the first notification. Clicking a notification only
  activates the app: `tauri-plugin-notification` has no click callback on
  desktop, so `focus_turn` is reserved in the contract but not sent yet.
  In a dev build macOS shows the notifications as Terminal's.
- **Reveal / open** (`reveal_path`, `open_path`): a workspace-relative path,
  resolved through the same confined view as the agent's tools (no `..`
  escape, no symlink, `path_deny` refused). `open_path` also refuses
  anything the OS would run rather than show (app bundles, scripts,
  installers, executables); reveal it instead.

## Logs

One log file per install, for support (`src-tauri/src/logging.rs`,
`tauri-plugin-log`):

| OS | File |
| --- | --- |
| macOS | `~/Library/Logs/ai.elitea.desktop/elitea.log` |
| Windows | `%LOCALAPPDATA%\ai.elitea.desktop\logs\elitea.log` |
| Linux | `~/.local/share/ai.elitea.desktop/logs/elitea.log` |

The same lines go to stdout (visible when the binary is run from a
terminal). Level: info in release builds, debug in debug builds; HTTP/TLS
internals (`hyper`, `reqwest`, `rustls`, …) only from warn. The file is
rotated at 5 MiB, keeping one previous file.

What is in it: the host's own messages, and from the UI (through
`plugin:log|log`, the webview's only log permission — it can write, never
read) `console.error`/`console.warn`, uncaught errors and unhandled
rejections, and at debug every route transition (path, router status,
matched routes) and every failed or slow (> 2 s) request to the deployment
(method, path, status, time). Never request bodies or headers. Every message
is scrubbed twice — in the webview (`shared/desktop/diagnostics.ts`) and in
the host's formatter: bearer tokens, `Authorization` values,
`access_token`/`refresh_token`/`code`/`password`-style parameters and the
query string of any URL are replaced with `[redacted]`.

To collect: quit Elitea, zip the folder above, attach it to the report.

## Registering the client

The deployment must list this client in its `native_clients` configuration:
client id, display name and the loopback redirect URI
`http://127.0.0.1/callback`, registered without a port (any port is accepted
at sign-in, RFC 8252 §7.3). The path must match exactly: `http://127.0.0.1/`
is refused with "the app's return address is not registered". The default client id is `desktop`. To use another one, in order of
precedence: the `ELITEA_DESKTOP_CLIENT_ID` environment variable at run time,
`client_id` in `settings.json` in the app config directory, or
`ELITEA_DESKTOP_CLIENT_ID` at build time.

## Security model

- **The stored sign-in is a file, not the OS keychain** — the same model as
  `~/.aws/credentials` or the Claude desktop app. `credentials.json` sits in
  the app config directory: `~/Library/Application Support/ai.elitea.desktop/`
  (macOS), `$XDG_CONFIG_HOME/ai.elitea.desktop/` (Linux),
  `%APPDATA%\ai.elitea.desktop\` (Windows). On Unix the directory is `0700`
  and the file is created `0600` from the start, written to a temp file in the
  same directory, fsynced and renamed into place (no partially written or
  briefly world-readable file). A file of yours that group or others can only
  read (a backup restore that lost the mode) is narrowed to `0600` with a
  warning. A file that is a symlink, not a regular file, owned by another
  user, or group/world-writable is never read, followed or written over
  (logged); sign-in then says to open **Help › Run Diagnostics…**, whose
  repair moves that entry aside (into `credentials.json.broken-<time>-<random>/`,
  never a symlink's target) so you can sign in again. Sign-out and wipe delete it once no pending
  revoke is left. The trade-off, honestly: it is protected by file
  permissions and disk encryption (FileVault, BitLocker, LUKS), not by
  per-application keychain ACLs, so any process running as you can read it —
  as it can read your AWS or SSH credentials. In exchange an unsigned or
  ad-hoc-signed build (every dev build) no longer asks for the login password
  on every keychain read. The keychain item older builds wrote is never read
  (reading it is what prompted); see "Upgrading from a keychain build".

- **Local tools cannot read the app's own data or your credentials.** The
  config, data, log and cache directories the app resolves once at launch
  (wherever `XDG_*` or the platform puts them; one that cannot be resolved
  is logged), the default locations under the bundle identifier from
  `tauri.conf.json` (Application Support, Caches, Logs, WebKit,
  HTTPStorages, Preferences, saved window state; `~/.config`,
  `~/.local/share`, `~/.cache`) as a fallback, and your credentials
  (`~/.ssh`, cloud, git and browser credentials, the keychains) form one
  deny list per session. The OS sandbox of every command, the app's own
  git, the file tools (`read_file`, `list_tree`, `search_files`,
  `read_document`) and the local index all honour it, even for a workspace
  opened at your home folder: the stored sign-in, the history, other
  workspaces' checkpoints and indexes stay out of reach, while the
  session's temporary directory inside them stays usable. A command whose
  sandbox cannot hide them (Linux with only Landlock, a workspace too large
  for bubblewrap's walk, no sandbox) is asked every time, and its result
  says why. Indexes built before this list existed are rebuilt before they
  answer.

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
  through the host's `http_fetch` command (`src-tauri/src/net.rs`, IPC.md
  "Network"): a Rust-side request, so no CORS change is needed on the
  deployment. It only reaches the connected deployment's origin (granted at
  launch for the stored one, and on every connect; origins accumulate per
  process, so one connected earlier stays reachable until the app restarts),
  never follows a redirect, drops the fetch spec's forbidden headers and keeps
  no cookies. The bearer token is still attached only to the current origin
  (checked in the webview's HTTP core and SSE client).
- **One HTTP client.** Every host call (discovery, the token endpoint, the D0
  platform and model calls) and every webview request share one pooled
  `reqwest` client (keep-alive, HTTP/2). It is built once, on a background
  thread at launch: building a client loads the TLS roots from the system
  trust settings (85–470 ms, serialised across the process), which the HTTP
  plugin used before did on every request, along with a new TCP + TLS
  connection. Deadlines are per request (10 s token/discovery, 90 s platform
  and model), never client-wide, so SSE streams stay open.
- **Launch.** When a session is stored, the host starts the launch's token
  refresh in its setup, while the window and the page load; the page's first
  token request joins it, and the refresh token still rotates once.

### Diagnostics

**Help › Run Diagnostics…** (any screen, signed in or not) and **Settings ›
Troubleshoot** open the Doctor (`src-tauri/src/doctor.rs`). It checks the
stored sign-in (`credentials.json`: regular file, yours, `0600`, parses), the
settings, data and logs folders (`0700`, yours, writable), the thread history
(opens, `PRAGMA integrity_check`, schema version), `workspaces.json` (parses;
every folder still there), each workspace's local code index (owner-only,
`PRAGMA quick_check`, schema version; rebuilt from the folder when damaged, once
confirmed), the deployment (reachable, discovery valid), the
session (a refresh works), the client policy's `local_work.allowed` (and how
an administrator turns it on) and the sign-outs still waiting to reach the
server. Each problem it can repair has a **Fix** button: restrict a file to
`0600` / a folder to `0700` when it is yours, move a damaged, foreign or
symlinked file aside into a new folder `<name>.broken-<YYYYmmdd-HHMMSS-mmm>-<random>/`
beside it, under its own name (the entry itself, never a link's target; an
earlier copy is never overwritten) so the app starts afresh — the thread
history goes with its `-wal` and `-shm` into one such folder after its writer
stopped and its connections closed, and a new history starts at once —, drop vanished folders from the
workspace list, retry the waiting sign-outs. Nothing is repaired without
the click; every repair is logged (paths and modes, never contents). At
launch the app runs the file checks and, when one fails, shows a small
"Something needs attention — Run Diagnostics" notice instead of failing
silently.

### Upgrading from a keychain build

Builds before the credentials file kept the device session in the OS keychain
(service `ai.elitea.desktop`, account `device-session`). The app does not read
or migrate that item, by design: any keychain access is what asked for the
login password on every launch of a re-signed build. After upgrading:

1. **Sign in again.** The new build starts signed out.
2. **Revoke the old device session** (optional, recommended): on the web, open
   **Settings › Devices** (`/settings/devices`) and revoke the older entry for
   this computer (`DELETE /auth/native/devices/{id}`); an admin can do the same
   from **Users › Devices**. Left alone, its refresh token stops working once
   it has been idle for the server's refresh idle TTL
   (`ELITEA_NATIVE_REFRESH_IDLE_TTL`, default 30 days, `720h`), or earlier when
   `ELITEA_NATIVE_SESSION_MAX_LIFETIME` is set.
3. Delete the stale `ai.elitea.desktop` item in Keychain Access if you like.

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
