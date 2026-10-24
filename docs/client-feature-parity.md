# Client feature parity

This table lists the user-facing features of the three Elitea clients:

- **Web**: `apps/elitea-web` in the browser.
- **Desktop**: `apps/elitea-desktop`, the Tauri host with the `desktop` Vite mode of `apps/elitea-web`.
- **Mobile**: `apps/elitea-mobile`, the Expo app "Elitea AI".

Update the table in the pull request that changes a feature on one client. The
pull request template asks for it. `scripts/ci/check-client-feature-parity.mjs`
checks the table in `no-binaries.yml`, which runs on every change.

## Rules

- Give each row a status for all three clients.
- Use one of three statuses:
  - `shipped`: the feature is on `main`.
  - `planned (#123)`: the feature is planned. Name the issue that tracks it.
  - `n-a: <reason>`: the feature does not apply to this client. Give the reason.
- Name a contract version in the feature name when the feature came with one.
- Keep one row for each feature. Do not split a feature across rows.

What the clients share in code is in `libs/ts/client-core/README.md`. The API
contract is shared through `services/elitea-main/api/openapi/v2.yaml` and the
clients generated from it.

## Features

| Feature | Web | Desktop | Mobile | Notes |
| --- | --- | --- | --- | --- |
| Chat with agents and models | shipped | shipped | shipped | |
| Streamed answers with tool activity | shipped | shipped | shipped | Stream parsing is shared: `@elitea/client-core/stream`. |
| Approvals and clarifying questions (HITL pauses, contract 1.2) | shipped | shipped | shipped | |
| Stop a running turn (contract 1.3) | shipped | shipped | shipped | |
| Regenerate an answer | shipped | shipped | shipped | |
| File attachments | shipped | shipped | shipped | |
| Rename, pin and delete chats | shipped | shipped | shipped | Mobile queues the actions offline. |
| Archive and Archived chats (contract 1.10) | shipped | shipped | shipped | Mobile queues Archive and Unarchive offline; Archived chats is online only. |
| Fork a conversation (contract 1.11) | shipped | shipped | shipped | Mobile forks online only (never queued). |
| Activity status and read markers in chat lists (contract 1.12) | shipped | shipped | shipped | Mobile does not follow the project event stream: transitions arrive with the delta sync and the list's refetch on foreground, not live `conversation_activity` events. Mobile's "N needs you" counts only the rows it has paged in: its sections (Pinned, Today, ...) are drawn on the device from the flat `/conversations` listing, which carries no group counts, so it cannot use the server's whole-group `needs_you` the web and desktop folder/date groups show. |
| Project skills in the composer's skill menu (contract 1.19) | shipped | shipped | n-a: the mobile composer has no skill menu | Web picks them in the `~` menu, desktop in the `/` menu. Both send ids only; the server (web) or the host (desktop) reads the skill under the person's permissions. At most five skills and 64 KiB of instructions in a turn. |
| Save a memory from the agent (contract 1.20) | n-a: the web saves memories in Settings > Memory | shipped | n-a: the mobile app only lists and deletes memories | The desktop's `save_memory` tool (#1233) writes through `createMemory`. |
| Notifications | shipped | shipped | shipped | |
| Discover public agents | shipped | shipped | shipped | |
| Workspace brand (ADR-0024 runtime branding) | shipped | shipped | shipped | The default mark and identity are shared: `@elitea/client-core/brand`. |
| Agent, pipeline, toolkit and MCP editors | shipped | shipped | n-a: authoring needs a large screen; the mobile app runs agents and does not edit them | |
| Native right panel (inspector) | n-a: the browser layout keeps its in-page panels | shipped | n-a: the phone layout has no side panel | Desktop host UI. |
| Local work (local agent over a folder) | n-a: the agent runs in the desktop host on the laptop | shipped | n-a: the agent runs in the desktop host on the laptop | Web and mobile list Local work threads. They are read-only while the laptop runs them. |
| Offline cache (encrypted copy on the device) | n-a: the browser keeps no copy of the workspace data | n-a: the desktop webview keeps no copy of the workspace data | shipped | Mobile delta sync (`changes_since`) and the offline outbox. |
| Home screen widgets and Live Activities | n-a: no home screen surface in a browser | n-a: no widget surface in the desktop host | shipped | |
| QR connect to a workspace | n-a: the browser opens the workspace URL | n-a: the connect screen takes the workspace URL | shipped | |
| Admin Schedules: switch platform routines on or off and change their schedules (#1369) | shipped | n-a: platform administration is the web console's job | n-a: platform administration is the web console's job | Admin routes are outside the client contract, which stays at 1.20. |
