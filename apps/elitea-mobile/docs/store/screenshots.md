# Screenshot plan

Store screenshots of the redesigned app (design system: docs/design-system.md;
screens: docs/screens.md). Capture them from a **Release** build signed in to
the demo workspace (listing.md "Review notes"), never from the dev-only design
gallery routes (`/design-*`) in a release listing: Apple requires screenshots
to show the actual app (guideline 2.3.3).

## Required sizes

The app sets `supportsTablet: true`, so the App Store needs iPad screenshots too.

| Store | Device class | Pixels (portrait) | Count |
|---|---|---|---|
| App Store | iPhone 6.9" (iPhone 16/17/18 Pro Max class) | 1320 x 2868 (1290 x 2796 accepted) | 3 to 10, required |
| App Store | iPhone 6.5" | 1284 x 2778 (1242 x 2688 accepted) | optional: scaled from 6.9" when absent |
| App Store | iPad 13" | 2064 x 2752 (2048 x 2732 accepted) | 3 to 10, required while `supportsTablet` is on |
| Google Play | Phone | 1080 x 1920 to 1440 x 3120, 9:16 to 9:20, PNG or JPEG, max 8 MB | 2 to 8, at least 4 for promotion |
| Google Play | 7" and 10" tablet | 1200 x 1920 / 1600 x 2560 | optional; needed for the "designed for tablets" badge |
| Google Play | Feature graphic | 1024 x 500, no transparency | 1, required |
| Google Play | App icon | 512 x 512 PNG, 32-bit | 1, required (from assets/images/icon.png's source) |
| App Store | App preview video | optional, 15 to 30 s | skip for v1 |

Check App Store Connect's "Screenshot specifications" page on the day: Apple
moves the required device class with new hardware.

## The set (same order in both stores)

Light appearance for the main set; one dark frame shows dark mode. Each frame
has one short caption above the device (sentence case, at most six words), on
the background token from docs/design-tokens.json.

| # | Screen (docs/screens.md) | State to stage | Caption |
|---|---|---|---|
| 1 | Home (§2) | greeting, action cards, recent chats, Zefirca | "Your workspace's AI agents, on the go" |
| 2 | Chat thread (§4) | a finished answer with formatting and a code block, action row visible | "Answers stream in live" |
| 3 | Chat thread, tool steps / approval (§4) | an agent waiting for approval (HITL card) | "Approve what your agents do" |
| 4 | Discover (§6) | categories, search, agent cards | "Find the right agent" |
| 5 | Lock screen widgets + Live Activity (iOS only) | counts widget and a running answer | "Know what's waiting" |
| 6 | Connect / sign-in (§1) | workspace confirm with the workspace's branding | "Sign in with your company account" |
| 7 | Settings, security (§8) | Face ID lock, offline copy, devices | "Locked to your device" |
| 8 | Home, dark (§2) | same as 1 in dark | "Easy on the eyes" |

iPad: frames 1, 2, 4, 7 in portrait (the layout is the phone layout scaled;
revisit if a split view is built).

Android: frames 1, 2, 3, 4, 6, 7 (no widget or Live Activity frame until the
Android surfaces exist). Feature graphic: Zefirca bust (assets/images/mascot/)
left, app name and "AI agents for your workspace" right, on the brand gradient.

## Data hygiene

- Demo workspace only, with invented names (no customer or employee data, no
  real email addresses); the workspace name and branding are the publisher's.
- No real tokens, server addresses of customers, or internal hostnames in
  frame.
- Status bar: 9:41, full battery, full signal (`xcrun simctl status_bar <udid> override --time 9:41 --batteryState charged --batteryLevel 100 --cellularBars 4`).
- No "Elitea" logo or wordmark in any frame (trademark, docs/open-questions.md §2).

## How to capture

1. Build `preview` (or a local Release build) and sign in to the demo workspace.
2. iOS Simulator at the exact device class (an iPhone 6.9" and an iPad 13"
   simulator): `xcrun simctl io <udid> screenshot frame-N.png`. Tapping:
   `axe tap --tap-style physical` (docs/device-verification.md notes).
3. Android emulator (once Android is built): `adb exec-out screencap -p > frame-N.png`.
4. Add captions and backgrounds in a design tool (Figma frame per size), export
   at exact pixels, sRGB, no alpha for the App Store.
5. Keep the sources and exports outside the repository (binary assets); record
   the app version and build number they were taken from.
