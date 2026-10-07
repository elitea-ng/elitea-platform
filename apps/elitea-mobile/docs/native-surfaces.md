# Native surfaces (iOS)

Widgets, the Live Activity and the Siri / Shortcuts intents. All three are
**outside the app**: they show on the home screen, the lock screen, the Dynamic
Island and in Siri, often while the device is locked. What they may show is set by
the workspace's client policy (client contract 1.3, see `src/security/policy.ts`).

| Surface | Shows by default | Shows more when | Wiped on |
|---|---|---|---|
| Widgets (`ZefirCounts`) | counts: waiting for you, unread, running | `allow_system_surfaces`: the workspace host and the titles of the waiting conversations | log out, revocation, workspace switch (signed-out snapshot) |
| Live Activity (`ZefirTurn`) | "Your agent is answering" (then "answered", "needs you" or "stopped") and the phase | `notification_preview: title` AND `allow_system_surfaces`: the conversation title | the turn ending (after a short linger), leaving memory while running, a wipe or switch; a new process ends leftovers |
| Siri / Shortcuts | the three intents, no agent names | `allow_system_surfaces`: the first agents of the selected project as `AgentEntity` | log out, revocation, switch (empty catalog, pending invocations dropped); an invocation made while signed out is dropped at once, one older than 5 min is never acted on |

Message content, tool names and credentials never reach any of them.

## Widgets

`src/features/surfaces/`. One WidgetKit extension (`ExpoWidgetsTarget`, bundle id
`ca.zefir.agent.widgets`) built by `expo-widgets` from `app.config.ts`. The app writes
a snapshot (`surfaceSnapshot.ts`) to the App Group `group.ca.zefir.agent`:

- after a sync changed stored data (`subscribeSyncChanges` in `src/sync/appSync.ts`),
  when the offline store opens, when the unread count changes and when a live turn
  starts, pauses or ends (`useWidgetSnapshot.ts`, mounted in the signed-in layout);
- for the **active workspace and its selected project** only;
- "waiting for you" = conversations whose newest answer waits for a decision
  (`pendingApprovals.ts`, from the offline store) plus turns paused in this run of the
  app. With no offline store (`offline_retention_days: 0`) only the live part is known;
- the snapshot holds no `null` (UserDefaults stores none, and the native module refuses
  a null prop): the host is `''` when titles are not allowed;
- a snapshot with running answers carries a second timeline entry ten minutes later
  without them: there is no background execution, so the app could not correct it.

The layout (`CountsWidget.tsx`) runs in the extension's own JS runtime: only
`@expo/ui/swift-ui` views and its props. Families: `systemSmall`, `systemMedium`,
`accessoryInline`, `accessoryCircular`, `accessoryRectangular`.

A tap opens `agentzefir://surface/<target>?ws=<slot>` (`surfaceLinks.ts`).
`+native-intent` turns it into the screen when the slot is the active workspace, else
the app's start. A conversation link also names its project (`&p=<id>`): after a
project change it opens the start, never the thread under another project. The device lock covers it like any other route, and the conversation
list stays beneath the screen it opens (Back works).

## Live Activity

`turnActivityController.ts`, `TurnActivity.tsx`. LOCAL only: no push token is
requested (push-updated activities are #12).

- Started when an answer has been running for 5 s, only in the foreground; at most two.
- Updated when the phase changes (working, using a tool, writing), not per frame.
- Ended on done / failed / stopped (lingers 2 min) or a pause for a decision (15 min).
  The open thread drops a finished turn from memory as soon as the persisted copy is
  back; the lingering activity stays (a new turn in that conversation ends it).
- Suspended or locked app: marked **approximate** ("Open the app for the latest") and
  stale at once; back to the live state on return.
- Ended at once when a turn leaves memory while still running, on a wipe or switch,
  and at the next launch for anything a killed process left. A sign-out ends them
  before the server revoke (an early wipe hook, like the widget snapshot and the Siri
  catalog), so a kill during "Signing out..." leaves no title on the lock screen.

## Siri and Shortcuts

`app-intents/*.swift` (compiled into the app target through Expo inline modules) and
`src/features/intents/`. **Open-app intents only**: each one opens the app and hands
an invocation to JS; Siri gets no dialog and no value back.
`authenticationPolicy = .requiresAuthentication` asks for an unlocked device, and the
app's own lock still covers the screen the intent opens.

| Intent | Phrase examples | Opens |
|---|---|---|
| New Chat (`NewChatIntent`, optional agent) | "New chat in Agent Zefir", "New chat with <agent> in Agent Zefir" | `/new`; with an agent of the selected project, that agent's conversation once the app is unlocked |
| Ask Agent (`AskAgentIntent`, optional agent, optional question) | "Ask Agent Zefir", "Ask <agent> in Agent Zefir" | as New Chat; a question typed into a Shortcut goes in the composer, **never sent** without the user tapping Send. By voice the composer opens empty: Siri never asks for the question (see below) |
| Open Notifications | "Open Agent Zefir notifications" | `/notifications` |

`AgentEntity` ids are `<workspace slot>:<project id>:<agent id>`: a shortcut saved for
one workspace or project opens the picker of the active one instead of another
workspace's agent. Invocations are removed from the queue **before** their screen
opens (at most once: a replay must not start a second conversation).
**Siri never collects the question.** A required text parameter, or a
`requestValueDialog`, makes Siri ask "What do you want to ask?" and transcribe the
answer with its own recogniser, which can run on Apple's servers depending on the
device, the language and the Siri settings, with no workspace control. The app's
speech is on the device only (docs/design.md), so the question is optional, has no
dialog, and only a value the user put in a Shortcut fills it
(`scripts/app-intents-no-siri-dictation.test.mjs` holds every intent to this).
Residual: a user-built Shortcut that dictates its own text (a "Dictate Text" action,
or "Ask Each Time" run through Siri) uses Apple's recogniser by the user's own
choice, and typed Shortcut text passes through the Shortcuts app.

The agent and the question reach `/new` (and the thread's composer) through an
in-memory hand-over (`intentHandoff.ts`) under a random one-time id, never as route
params: `agentzefir://new?agent=…&prefill=…` from a web page or another app opens the
plain picker and types nothing.

## Verifying

Unit tests cover the snapshot, links, writer, pending scan, Live Activity controller,
intent routing and catalog; the widget and Live Activity layouts are evaluated from
their serialised `'widget'` source (`src/test/widgetRuntime.ts`). On a device or the
simulator, follow the "Native surfaces" section of
[device-verification.md](device-verification.md).
