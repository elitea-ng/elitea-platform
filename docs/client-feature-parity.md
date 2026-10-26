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
| Steer a running turn: send while it works (contract 1.27) | shipped | shipped | shipped | A message sent mid-turn is queued against the running answer and read at the next safe point of the same turn. The transcript shows it as waiting, then delivered; the author can edit or remove a waiting one, or stop and send. A message the turn did not read becomes the next message (texts joined by a blank line): the client takes it with the hand-off operation and sends it, and the server starts that turn itself when no client did within about 10 seconds, or when the client took it but its send never arrived (about a minute, same question id). The server never continues a turn the user stopped or that failed: the client offers to send the waiting text as the next message. Clients order steering by the server's `queue_order`. The desktop's ACP agent queues a prompt that arrives during a turn into that turn the same way. A pipeline refuses steering: the web queues the text for after the turn, mobile keeps Send disabled with a notice. The desktop steers its local turns too. The folding rules are shared: `@elitea/client-core/stream` (`steering.ts`). |
| Regenerate an answer | shipped | shipped | shipped | |
| File attachments | shipped | shipped | shipped | |
| Rename, pin and delete chats | shipped | shipped | shipped | Mobile queues the actions offline. |
| Chat names from the first exchange (contract 1.24) | shipped | shipped | shipped | The server's model names a chat that still has its automatic name. Desktop Local work threads can also be renamed. |
| Archive and Archived chats (contract 1.10) | shipped | shipped | shipped | Mobile queues Archive and Unarchive offline; Archived chats is online only. |
| Fork a conversation (contract 1.11) | shipped | shipped | shipped | Mobile forks online only (never queued). |
| Activity status and read markers in chat lists (contract 1.12) | shipped | shipped | shipped | Mobile does not follow the project event stream: transitions arrive with the delta sync and the list's refetch on foreground, not live `conversation_activity` events. Mobile's "N needs you" counts only the rows it has paged in: its sections (Pinned, Today, ...) are drawn on the device from the flat `/conversations` listing, which carries no group counts, so it cannot use the server's whole-group `needs_you` the web and desktop folder/date groups show. |
| Project skills in the composer's skill menu (contract 1.19) | shipped | shipped | n-a: the mobile composer has no skill menu | Web picks them in the `~` menu, desktop in the `/` menu. Both send ids only; the server (web) or the host (desktop) reads the skill under the person's permissions. At most five skills and 64 KiB of instructions in a turn. |
| Save a memory from the agent (contract 1.20) | n-a: the web saves memories in Settings > Memory | shipped | n-a: the mobile app only lists and deletes memories | The desktop's `save_memory` tool (#1233) writes through `createMemory`. |
| Rewind a conversation to an answer, fork idempotency (contract 1.21) | n-a: the web chat forks only (contract 1.11) | shipped | n-a: mobile forks only (contract 1.11) | Desktop's "Rewind to Here" (#1234) sends `mode: rewind` with a `fork_id`; the rewind keeps the abandoned turns as `forked_from.branch_summary`, and its source is listed as "Earlier attempt". |
| Web search and fetch, host lookups in a local turn; tool usage in analytics (contract 1.22) | n-a: web chat agents use the toolkits through the cloud runtime, not a local work policy | shipped | n-a: mobile runs no local turns | The desktop offers the `web` and `host_intel` toolkits only while the policy's `web` / `host_intel` keys are on (#1238); a host lookup is asked once per app session. Admin LLM proxy usage shows tool rows and tool prices. |
| Nested agents of a local turn run as cloud child turns; a pipeline runs in the cloud from a workspace (contract 1.23) | n-a: the web chat runs nested agents in the cloud runtime already | shipped | n-a: mobile runs no local turns | The desktop offers each nested agent of the turn's agent as a tool that runs it as a hidden cloud child conversation (#1239), followed until it settles, its approve/reject pauses on the approval card, stopped with the turn. A pipeline picked in a workspace runs as a cloud conversation that opens in chat. |
| Notifications | shipped | shipped | shipped | |
| Discover public agents | shipped | shipped | shipped | |
| Workspace brand (ADR-0024 runtime branding) | shipped | shipped | shipped | The default mark and identity are shared: `@elitea/client-core/brand`. |
| Agent, pipeline, toolkit and MCP editors | shipped | shipped | n-a: authoring needs a large screen; the mobile app runs agents and does not edit them | |
| Create an agent in a conversation (contract 1.28) | shipped | shipped | shipped | ADR-0037. The builder streams a draft, the person reviews and saves it, and it is listed with the other agents at once. Desktop also offers the local branch in a folder session: the session agent writes an `AGENTS.md` behind the approval card. Mobile offers the Elitea branch only and does not edit an agent after it is saved. |
| Native right panel (inspector) | n-a: the browser layout keeps its in-page panels | shipped | n-a: the phone layout has no side panel | Desktop host UI. |
| Local work (local agent over a folder) | n-a: the agent runs in the desktop host on the laptop | shipped | n-a: the agent runs in the desktop host on the laptop | Web and mobile list Local work threads. They are read-only while the laptop runs them. |
| Context breakdown, idle compaction and bypass mode for Local work | n-a: the agent runs in the desktop host on the laptop | shipped | n-a: the agent runs in the desktop host on the laptop | The Context tab shows the parts of the context. Bypass mode allows what would be asked. |
| Take back a cloud thread up to turn k, then the rest (contract 1.25) | n-a: the take back applies the cloud turns to the laptop's folder | shipped | n-a: the take back applies the cloud turns to the laptop's folder | A release says how many cloud turns it did not take; the desktop can take them back until the cloud copy's return grace ends. Web and mobile only see the state. |
| Files changed of a cloud thread (contract 1.26) | shipped | n-a: the desktop sees the cloud's changes in the take-back review, before it applies them | planned (#1187) | The web chat of a cloud thread lists the files each cloud turn changed and what changed since the handoff, with a diff per text file. |
| Pushes and pull requests from a cloud thread through the thread's GitHub or GitLab credential (contract 1.29) | shipped | shipped | shipped | The owner chooses a GitHub or GitLab configuration (the project's, a shared one, or their own) in the web chat's cloud notice, the desktop's cloud-thread banner and the mobile thread's cloud notice; it is a reference, redeemed only for the thread's cloud turn and never in the sandbox. `git_push` pushes the thread's commits to a new branch and `open_pull_request` opens a pull request (a merge request on GitLab); both ask on the approval card on every client unless the policy lets them run without it. Their rows link the branch, pull request or merge request on every client (desktop through the system browser). |
| Offline cache (encrypted copy on the device) | n-a: the browser keeps no copy of the workspace data | n-a: the desktop webview keeps no copy of the workspace data | shipped | Mobile delta sync (`changes_since`) and the offline outbox. |
| Home screen widgets and Live Activities | n-a: no home screen surface in a browser | n-a: no widget surface in the desktop host | shipped | |
| QR connect to a workspace | n-a: the browser opens the workspace URL | n-a: the connect screen takes the workspace URL | shipped | |
| Admin Schedules: switch platform routines on or off and change their schedules (#1369) | shipped | n-a: platform administration is the web console's job | n-a: platform administration is the web console's job | Admin routes are outside the client contract, which stays at 1.20. |
