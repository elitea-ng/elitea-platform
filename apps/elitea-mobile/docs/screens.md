# Agent Zefir: target screens

Layouts for each main screen. The tokens are in `design-tokens.json` and the components in
`design-system.md` (section numbers such as §4.5 refer to that file). The wireframes are
schematic, at a 390×844 pt phone. `G` marks the gradient ground and `[glass]` a glass
surface.

**Light and dark.** Every screen uses the same layout in both schemes. Only tokens change:

| | Light | Dark |
|---|---|---|
| Gradient | pink mist → lavender → mint | plum-black → indigo-black → green-black |
| Glass | white α .72 | `#2A2631` α .72 |
| Accent | Zefir rose `#B4306A` (or the corrected workspace accent) | `#FF8CB8` with dark `accentOn` |
| Status bar | `dark-content` | `light-content` |

- The status bar is set from the scheme via `expo-status-bar` `style="auto"`.
- Android is edge-to-edge in both schemes, with a transparent system navigation bar. The
  content pads by the inset.

## Route map (target)

```
/connect                 stack root when no workspace (existing route)
/connect/confirm         ConnectConfirm (existing component, own screen)
/sign-in                 (existing)
/(tabs)/(home)           Home, the `/` route and the first tab (shipped)
/(tabs)/chats            the conversation list, `/chats` (shipped)
/(tabs)/discover         NEW: agents catalogue (fetchAgents)
/(tabs)/settings         (existing settings, regrouped)
/c/[id]                  chat thread: stack above the tabs, tab bar hidden
/voice                   full-screen modal
/new                     agent picker: sheet
/notifications           pushed from the bell
/projects, /workspaces   project and workspace switchers: sheets with detents (shipped)
```

The lock gate (`LockGate`) and the privacy cover sit above everything, unchanged in
behaviour and restyled (§9).

---

## 1. Connect, confirm, sign-in

### 1a. Connect (welcome)

```
G ───────────────────────────────
           (status bar)

        [ mascot full, 240 ]          decorative, gentle tilt once
           on mint pedestal

        Meet Agent Zefir              title1, centred
   Your workspace's AI agents,        callout, textSecondary
       in your pocket.

  ┌──────────────────────────────┐
  │ ⌖  Scan workspace QR code    │    filled pill (accent), 50/48
  └──────────────────────────────┘
  ┌──────────────────────────────┐
  │    Enter server address      │    tonal pill (accentSoft)
  └──────────────────────────────┘
     Privacy · Help                   caption, text buttons
──────────────────────────────────
```

- When "Enter server address" is chosen, the buttons are replaced in place by a text
  field labelled "Server address", placeholder `acme.elitea.ai`.
  - `keyboardType="url"`, `autoCapitalize="none"`, `autoCorrect={false}`,
    `textContentType="URL"`.
  - The field has an inline "Continue" button.
  - Errors from `parseConnectLink` show under the field in `danger`, with an icon.
- **MDM pre-set:** the screen skips straight to Confirm.
- **Accessibility type sizes:** the mascot shrinks to the 96 bust and the content
  scrolls.
- The palette is always Zefir here, because no brand is applied before confirmation.

### 1b. Confirm workspace (existing `ConnectConfirm`)

```
G ───────────────────────────────
 ‹ Back
          ┌──────────┐
          │  LOGO 56 │          workspace logo mark (after pack load), else
          └──────────┘          monogram tile on surface
        Acme Corp AI            title2: product.name from the pack
     acme.elitea.ai             headline, SYSTEM FONT, textPrimary (never truncated;
                                wraps; selectable)
 ┌────────────────────────────┐
 │ 🔒 Secure connection (TLS) │  surface card, footnote rows
 │ ✓ Supports Agent Zefir     │  (capabilities from discovery)
 └────────────────────────────┘
  Only connect to workspaces you trust.   footnote, textSecondary
 ┌────────────────────────────┐
 │        Connect             │  filled pill
 └────────────────────────────┘
          Cancel                text button
```

- **No mascot on this screen.** The workspace identity must dominate it (design-system
  §3.5 and §5).
- The pack's accent is **not** applied yet. Buttons are Zefir rose, so the workspace
  cannot style the trust decision.

### 1c. Sign-in

```
G ───────────────────────────────
  ┌──────┐   ┌──────┐
  │ bust │   │ LOGO │      two separate 64 pt tiles, 16 pt apart
  └──────┘   └──────┘
   Sign in to Acme Corp AI          title2
   acme.elitea.ai                   footnote, system font
 ┌────────────────────────────┐
 │  Continue with SSO         │     filled pill (brand accent now applies)
 └────────────────────────────┘
   You'll sign in on acme.elitea.ai   footnote
   in a secure browser window.
         Use a different workspace    text button (wipe-confirm flow)
```

- iOS shows its own sheet: '"Agent Zefir" Wants to Use "acme.elitea.ai" to Sign In'.
  Android opens a Custom Tab.
- **Progress:** the button shows a spinner and the label "Signing in…". Cancelling
  returns here silently.

---

## 2. Home (tab 1)

```
G ───────────────────────────────
 (●) avatar 36                [glass ◯ 🔔³]     bell 40, badge
  ┌ LOGO20 Acme Corp AI ▾ ┐                    workspace pill (glass) → /workspace
  └───────────────────────┘
 Hello, Alex                    display; "Alex" in accent
 What can I help with?          display, textPrimary

 [Agent: Research ▾] [🎙] [🖼] [📎]   chip row (§4.3)

 ┌──────────────┐ ┌──────────────┐
 │ (✎)      (↗) │ │ (◉)      (↗) │   action cards (glass, r28)
 │              │ │              │
 │ New chat     │ │ Talk         │
 └──────────────┘ └──────────────┘
 ┌──────────────┐ ┌──────────────┐
 │ (🖼)     (↗) │ │ (↺)      (↗) │
 │ Ask about a  │ │ Continue:    │
 │ photo        │ │ Q3 planning  │
 └──────────────┘ └──────────────┘

 Recent chats                     See all
 ┌──────────────────────────────────┐
 │ [av] Make my website after…   (↗)│   history rows (§4.4), max 5
 │      Research · 2h               │
 ├──────────────────────────────────┤
 │ [av] Start local film club…   (↗)│
 └──────────────────────────────────┘
          (space for tab bar)
[ Home ]  [ Chats ]  [ Discover ]  [ Settings ]
```

- **Project scope:** the workspace pill shows the *workspace*. When there are several
  projects, it reads "Acme Corp AI · Marketing" (the project replaces the old header
  `ProjectChip`).
- **Pull to refresh** reloads recents and notifications.
- **Chips:** the agent chip sets the target used by the cards and the composer. Mic opens
  Voice. Image and attach start a new chat with that picker.
- There is **no composer on Home**. The reference's home has none, and the cards and
  chips cover the entry points. This keeps Home clear of the keyboard.
- **Empty (no chats yet):** "Recent chats" is replaced by the empty state (mascot bust
  96, "Ask me anything", "Your chats show up here. Start one with New chat or your voice.").
  It has **no button of its own**: the New chat card and the chips above already start a
  chat, and a button that low sat under the floating tab bar at rest.
- **No active project** (none, or all suspended): the greeting and the workspace pill stay;
  the chips, cards and "Recent chats" are replaced by the mascot empty state "No active
  project", because every card would end at "No project selected".
- **Offline:** the `warningSoft` offline strip sits under the workspace pill. Cards still
  work (the outbox), Talk is disabled with the reason "Voice needs a connection", and the
  recent rows come from the encrypted cache.
- **iOS:** on scroll the greeting collapses and an inline "Home" title appears in a glass
  nav bar. The bell stays. **Android:** an M3 medium top app bar, which behaves the same
  way.
- **Dark:** the cards are glass `#2A2631` α .72 with a top hairline. The accent "Alex" is
  `#FF8CB8`.

## 3. Chat list (tab 2, "Chats")

```
 Chats                     [✎]          iOS large title + new-chat button;
 [🔍 Search chats          ]            Android: M3 top app bar + search bar
 [All] [Unread] [Pinned]                filter chips
 PINNED
 ┌──────────────────────────────────┐   inset-grouped list (surface)
 │ [av] Q3 planning            9:41 │
 │      Research: Here's the dr…  • │   unread dot
 ├──────────────────────────────────┤
 TODAY
 │ [av] Film club charter     Yesterday
 │      You: draft the bylaws        │
 └──────────────────────────────────┘
```

- Rows are grouped by Pinned, Today, Yesterday, This week and Older. The group headers
  use the footnote role, uppercase on iOS and sentence case on Android.
- **Avatar:** the agent icon if it has one, else the mascot bust (with the single-use
  rule, design-system §4.6 and §5), else a monogram.
- Swipe actions follow design-system §4.11 (as built: §13). Long-press opens Pin, Rename and
  Delete (the contract has no Archive). Pin and Rename need client contract 1.4.
- Search is over the titles and messages kept on the device (the encrypted store's FTS5
  index), the same online and offline: the workspace offers no conversation search. A message
  hit shows the matched passage under the title.
- **Empty:** the full mascot with [New chat]. **No results:** the bust with
  "Nothing found for '…'".
- **iPad (regular width, ≥ 700 pt: full screen, 2/3 Split View, a wide Stage Manager window):**
  two panes, this list on the left (about a third, 320–400 pt) and the selected chat on the
  right, with its own bar (title and actions); the navigation bar is the list's, with an inline
  title. A tap selects (highlighted, read as selected, no chevron); nothing selected shows "No
  chat selected" with New chat. Below 700 pt (1/3 and most 1/2 splits) it is the phone stack.
  The selection survives a change of width: collapsing pushes the open chat, and a chat opened
  on its own screen is the one selected when the two panes come back. It is per workspace and
  project and wiped with the session.

## 4. Chat thread (`/c/[id]`)

```
 ‹  [av] Research ▾        [⋯]        nav bar: agent avatar + name (tap → participants
    Acme Corp AI                       sheet); subtitle footnote = workspace/project
 ─────────────────────────────────    flat `background` (no gradient behind text)
                    ┌───────────────┐
                    │ Hey, can you  │  user bubble (accent fill, white text)
                    │ summarise…    │
                    └───────────────┘
 [av] Research · 9:41
 ┌───────────────────────────────┐
 │ Absolutely. Here's the …      │    agent bubble (surface + hairline)
 │ ▸ Used 2 tools ✓              │    tool steps, collapsed
 └───────────────────────────────┘
  ⧉  👍  👎  🔊              ↻       action row (last message gets regenerate)

 ┌ Approval needed ─────────────┐     approval card (§4.7), warning edge
 │ Research wants to run        │
 │ send_email on acme.elitea.ai │
 │ [Approve] [Reject] [Details] │
 └──────────────────────────────┘
 ┌──────────────────────────────────┐
 │ (+)  Message Research…   (🎙)    │   floating glass composer (§4.5)
 └──────────────────────────────────┘
```

- **Scrolling:** the list is inverted and anchored at the bottom. A "Jump to latest"
  glass circle (↓) appears when scrolled up and a new token arrives, with an
  accessibility announcement.
- **Header "⋯" menu:** Participants, Rename, Share transcript (copy), Delete.
- **Mentions:** `@` opens the mention picker above the composer, and `/` opens the slash
  picker (existing `SuggestionStrip`).
- **States:** the thinking indicator (mascot bust and dots), streaming (caret), error
  (inline `dangerSoft` row "Couldn't get a reply · Retry"), offline outbox messages
  (clock glyph and "Waiting to send"), and paused tool calls (approval card).
- **Android:** predictive back is on (`predictiveBackGestureEnabled: true`, app shell
  redesign): Android 16 shows the system back preview. The system back closes sheets first,
  then pops. The composer rides the IME insets under edge-to-edge
  (`react-native-keyboard-controller`).
- **Dark:** user bubbles are `#5B2541` with white text, and agent bubbles `#221F28` with
  no border.
- **As built** (`feat/redesign-chat`, design-system.md §10): the approval card closes the
  transcript inline; the header keeps Export and Participants as bar buttons (the "⋯" menu and
  the avatar title are not built); dictation's listening state is the voice-orb card above the
  composer rather than the full-screen `/voice` modal, which stays unbuilt.

## 5. Voice (`/voice`, full-screen modal)

```
G ───────────────────────────────
 (✕)      ( ● Listening )      (⋯)     close · status pill · menu
           Listening…                  title1
   "Summarise yesterday's meeting      transcript (body, textSecondary,
    and draft a follow-up"              live, ≤4 lines)

              ◯◯◯◯◯
            ◯  ORB  ◯                  220 pt orb, decorative
              ◯◯◯◯◯

 ┌──────────────┐ ┌──────────────┐     suggestion cards: shown only before speech
 │ (≋)      (↗) │ │ (✦)      (↗) │
 │ Read last    │ │ Continue in  │
 │ answer aloud │ │ text         │
 └──────────────┘ └──────────────┘
    (⌨)        ( 🎙 72 )        (✕)    keyboard · mic (accent when recording) · cancel
```

- **States:** idle ("Tap to talk"), listening (red dot + "Listening"), paused
  ("Paused"), processing ("Getting that ready…"), and error ("Didn't catch that · Try
  again").
- **Before first use:** a permission primer appears *before* the OS prompt, as a sheet
  with the mascot bust, one line explaining why, and [Allow microphone]. If permission
  was denied, the sheet shows a deep link to Settings.
  - This is blocked today: `app.config.ts` sets `microphonePermission: false`.
- The orb tint follows the corrected brand accent hue at stop 0 (decorative).
- **Dark:** the orb stops are `#C2477E`, `#6F5BB8` and `#2F8C7E` with a dimmer highlight.

## 6. Discover (tab 3)

```
 Discover                              large title
 [🔍 Search agents          ]
 [All] [Recent] [Favourites]           filter chips
 ┌──────────────┐ ┌──────────────┐
 │ [icon 48]    │ │ [bust 48]    │     agent cards (glass, r22)
 │ Research     │ │ HR helper    │     headline
 │ Finds and …  │ │ Answers pol… │     footnote, 2 lines
 │ [ Chat ]     │ │ [ Chat ]     │     tonal pill
 └──────────────┘ └──────────────┘
```

- **Shipped (client contract 1.3) as a tab**: the native tab bar (`(app)/(tabs)`) has
  Chats, Discover and Settings (Home comes with the home screen). `/discover` is the tab's
  root; `/discover/[id]` is pushed above the tabs, like a thread. The tab is there on every
  workspace (a changing tab set would remount the bar and every tab's content); below 1.3
  its screen says "Discover isn't available", and it waits while the workspace is not yet known. It lists the workspace's PUBLIC agents (`listPublicApplications`, 20 per page),
  not the project's own (those stay in `/new`); see `docs/design.md` "Discover".
- Target: data from `fetchAgents(projectId, search)`, 50 per page, loaded as you scroll.
- Tapping a card opens an agent detail sheet: icon (48), name, description, [Start chat],
  [Talk].
- **Scope:** only agents that a conversation can start with. Agent authoring is out of
  scope (`docs/design.md`).
- At accessibility sizes the grid becomes a 1-column list.

## 7. Notifications (`/notifications`, pushed from the bell)

```
 ‹  Notifications            Mark all read
 TODAY
 ┌──────────────────────────────────┐
 │ (●) [icon] Research finished     │  unread dot + bold title
 │      "Q3 planning" · 5 min       │
 ├──────────────────────────────────┤
 │     [icon] Approval needed       │  warning icon; tap → thread at the card
 │      send_email · 1h             │
 └──────────────────────────────────┘
```

- In-app only (push is out of the MVP). The list is fed by the existing
  `notificationStream`.
- Each row's label is "{title}, {detail}, {time}, unread".
- **Empty:** mascot bust (96), "You're all caught up".
- New items show a glass toast on any screen ("Research finished · View"). It is not
  shown while VoiceOver is focused in the composer. It is announced politely.

## 8. Settings (tab 4)

```
 Settings                               large title, inset-grouped
 ┌──────────────────────────────────┐
 │ (●) Alex Smith                   │  account row: avatar, name, email
 │     alex@acme.com                │
 └──────────────────────────────────┘
 WORKSPACE
 │ [LOGO] Acme Corp AI          ›   │  → /workspace sheet
 │        acme.elitea.ai            │  host in system font
 │ Project            Marketing ›   │
 APPEARANCE
 │ Theme       System · Light · Dark│  segmented control (iOS) / M3 segmented button
 │ App icon          Zefir      ›   │  → icon picker (A / B previews, confirm sheet)
 SECURITY
 │ Face ID lock            [on]     │  (policy may force on → disabled + footnote)
 │ Lock after         5 minutes ›   │  (server policy shown as read-only when set)
 OFFLINE & STORAGE
 │ Keep chats offline      [on]     │
 │ Clear offline data           ›   │
 ABOUT
 │ About Agent Zefir            ›   │  mascot full 160, version, privacy, support
 ┌──────────────────────────────────┐
 │           Sign out               │  danger text; confirm sheet explains the wipe
 └──────────────────────────────────┘
```

- **App icon picker:** two 64 pt previews ("Zefir" and "Zefir Night"), each with a
  radio. Choosing one shows a confirm sheet, then the OS changes the icon (iOS shows its
  alert; on Android the copy says "the app may restart").
  - When a self-hosted workspace connects and `productDefault` is false, a one-time
    suggestion banner offers Zefir Night.
- **Server-enforced policy rows** show "Set by Acme Corp AI" in a footnote. That text is
  the host or name, and it never comes from the pack's free-text fields.
- **Android:** M3 list items, with switches on the trailing edge and section headers in
  `accent` labelSmall (M3 style).

## 9. Workspaces & projects (`/workspace` sheet), and lock

```
 ─── (grabber)
 Workspace                                   title3
 ┌──────────────────────────────────┐
 │ [LOGO] Acme Corp AI           ✓  │  current; host on line 2
 │        acme.elitea.ai            │
 └──────────────────────────────────┘
 PROJECTS
 │ (•) Personal                     │  radio list, selected = accent check
 │ ( ) Marketing                    │
 │ ( ) Engineering                  │
 ┌──────────────────────────────────┐
 │  Switch workspace…               │  tonal button → wipe-confirm sheet:
 └──────────────────────────────────┘  "This removes chats, offline data and
                                         sign-in for acme.elitea.ai from this device."
```

- The MVP has one connected workspace. "Switch" therefore means disconnect, wipe, then
  Connect, following the existing wipe rules. The sheet makes that explicit, with a
  `danger` confirm.
- Detents are medium and large. On Android it is an M3 bottom sheet.

**Lock screen (`LockGate`):**
- the gradient ground
- the mascot bust (96)
- "Agent Zefir is locked" (title2)
- the workspace logo and host underneath (footnote)
- [Unlock with Face ID] / [Unlock with fingerprint] (filled pill).

**Privacy cover:** the same ground with only the bust and the logo, and no text from the
conversation.

---

## Platform checklist (applies to every screen)

- **Touch targets:** at least 44 pt (iOS) / 48 dp (Android). Every icon-only control has
  an `accessibilityLabel`, and toggles carry `accessibilityState`.
- **Text scales** with Dynamic Type and Android font scale. The layout switch is at 1.35
  (design-system §1.7).
- **Contrast:** every text pair proven ≥ 4.5 in both schemes. Brand accents are corrected
  before use (design-system §3.3).
- **Reduce motion, reduce transparency and increase contrast** are honoured (§1.3, §1.9,
  §3.4).
- **Native patterns:**
  - iOS: large titles, inset-grouped lists, form sheets, context menus, swipe actions,
    SF Symbols, native tabs (Liquid Glass on 26).
  - Android: M3 top app bars, navigation bar, bottom sheets, snackbars, ripple, Material
    Symbols Rounded, edge-to-edge, themed (monochrome) icon.
- **RTL:** layouts use `start`/`end`. Arrow glyphs mirror (`arrow.up.right` →
  auto-mirrored `arrow_outward`). Bubble sides swap.
- **Workspace host and logo** are visible on connect, sign-in, Home, Settings, the lock
  screen and approval cards. The mascot never overlaps them.
