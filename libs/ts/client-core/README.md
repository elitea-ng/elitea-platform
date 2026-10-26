# @elitea/client-core

Pure TypeScript shared by the Elitea clients:

- the web app and the desktop UI (`apps/elitea-web`, with its `desktop` Vite mode in the Tauri host `apps/elitea-desktop`)
- the mobile app (`apps/elitea-mobile`, Expo)

The package is private and has no runtime dependencies. It has no build step:
each app compiles the TypeScript source with its own toolchain. The API
contract is NOT here. It stays in `services/elitea-main/api/openapi/v2.yaml`,
and each app generates its own client from it.

## Contents

| Import | What | Used by |
| --- | --- | --- |
| `@elitea/client-core/stream` | `EventStreamParser`: the WHATWG `text/event-stream` parser for clients that read SSE with `fetch` | desktop (`shared/desktop/fetchEventSource.ts`), mobile (`api/sse/stream.ts`) |
| | `EXECUTION_EVENT`: the SSE event names of the execution replay stream | web (`shared/api/sse/executionEvents.ts`), mobile (`features/chat/frames.ts`) |
| | `FRAME_TYPE`: the chat stream frame vocabulary | web (`SocketMessageType` in `features/chat-messages/lib/chatStreamFrame.ts`), mobile (`turnReducer.ts`, `toolActivity.ts`) |
| | Steering (client contract 1.27, `stream/steering.ts`): `parseSteeringMessage` / `parseSteeringList` (an unknown state reads as `pending`), `steeringDropReason` (the `reason` of `steer.dropped`: `removed` or `superseded`, an unknown value reads as `removed`), `mergeSteering` (a higher `revision` wins and a final state beats `pending`, so a replayed `steer.*` event cannot undo an injection), `visibleSteering` (hides an item whose `message_id` is already a transcript row), `leftoverSteering` + `joinSteeringTexts` (the `turn_ended` messages of this client, joined with a blank line as the next turn's message), `steeringRetryAfterMs` / `steeringWaitSeconds` (the wait of a 429 `rate_limited`, from `retry_after_ms`), `STEERING_ERROR` codes and the 16 KiB bound | web (`features/chat-messages/model/useChatSteering.ts`), mobile (`features/chat/turnController.ts`) |
| | `ANSWER_DELTA_TYPES`, `TURN_FAILURE_TYPES`, `TOOL_CALL_TYPES`, `PAUSE_FINISH_REASONS`: the frame classes every client applies the same way | web (`chatStreamTurnEnd.ts`), mobile (`turnReducer.ts`, `toolActivity.ts`, `api/queries/messageTraces.ts`) |
| | `parseEventData`: `JSON.parse` of an SSE `data` field that never throws | web, mobile |
| `@elitea/client-core/brand` | `ELITEA_MARK_SVG`, `ELITEA_MARK_VIEWBOX`, `ELITEA_MARK_ASPECT`: the Elitea mark | mobile (`components/EliteaMark.tsx`) |
| | `DEFAULT_PRODUCT`, `DEFAULT_BRAND_HUE`: the default identity | web (`defaultPackSync.test.ts`), mobile (`EliteaMark.tsx`) |
| | `DEFAULT_PACK_ACCENTS`: the default pack's accent inputs | mobile (`theme/brandPack.ts`) |

## Brand: one source, derived copies

The canonical artwork is `src/brand/logo-mark.svg` and `src/brand/logo-full.svg`.
The canonical identity is `src/brand/identity.ts`. Edit only these files.

`scripts/gen-brand.mjs` writes the derived copies of the artwork:

- `src/brand/mark.gen.ts`: the mark as a string, for React Native `SvgXml`
- `apps/elitea-web/public/brand/{logo-mark,favicon,logo-full}.svg`: the files the default pack's `assets.*` name
- `apps/elitea-web/src/shared/ui/icons/svg/{logo-mark-icon,logo-icon}.svg`: `<LogoMarkIcon>` and `<LogoIcon>`

The default pack (`apps/elitea-web/src/shared/brand/tokens/default.pack.json`)
and `src/brand/defaultPack.gen.ts` are hand-maintained: the generator that
wrote them (`gen-brand-tokens.mjs`) read the `apps/elitea-ui` palettes and went
with that submodule. Edit them alongside `identity.ts`;
`apps/elitea-web/src/shared/brand/__tests__/defaultPackSync.test.ts` fails when
the three disagree.

Run the artwork generator after an edit:

```bash
node libs/ts/client-core/scripts/gen-brand.mjs
```

It has a `--check` mode, and `ci-web.yml` runs it. These values are the
product DEFAULT only. ADR-0024 runtime branding is unchanged: a workspace brand
pack replaces the name, the colours and the artwork at run time on every client.

## How each app consumes the source

| Tool | Configuration |
| --- | --- |
| Web `tsc`, Vite, Vitest | `tsconfig.json` `paths`: `"@elitea/client-core/*": ["../../libs/ts/client-core/src/*/index.ts"]`, through `vite-tsconfig-paths`. `vite.config.ts` adds the package to `server.fs.allow`. |
| Web and elitea-main images | The Containerfiles keep the repository layout (`/build/apps/elitea-web`) and copy `libs/ts/client-core/` next to it. |
| Mobile `tsc` | The same `paths` entry in `apps/elitea-mobile/tsconfig.json`. |
| Mobile Metro (Expo SDK 58) | `tsconfigPaths` (on by default) resolves the alias. `metro.config.js` adds the package to `watchFolders` and the app's `node_modules` to `resolver.nodeModulesPaths`, so a Babel helper that the transform adds to a package file resolves. |
| Mobile jest | `moduleNameMapper` for the alias, and `moduleDirectories` with `<rootDir>/node_modules` for the same reason. |

The source uses extensionless relative imports, because the apps do not set
`allowImportingTsExtensions`. Keep every file free of React, MUI, React Native,
storage and `fetch`.

## Checks

```bash
cd libs/ts/client-core
npm test                  # node:test on the TypeScript source (Node >= 24 strips the types)
npm run typecheck         # strict, with the web app's TypeScript and @types/node
npm run check:generated   # the derived artwork matches the canonical SVGs
```

`npm test` loads `test/register.mjs`, a resolve hook that maps an extensionless
import to its `.ts` file. The typecheck uses `apps/elitea-web/node_modules`, so
run `npm ci` in `apps/elitea-web` first. `ci-web.yml` (job `gates-static`) runs
all three. `ci-mobile.yml` compiles, tests and bundles the package through the
mobile app.

## Decisions (2026-10-21)

The investigation compared what each app implements today. Only logic that is
pure, equivalent in both apps, and a drift risk moved here.

### Shared

- **The SSE parser.** The desktop parser (`shared/desktop/sseParser.ts`) was a
  port of the mobile parser (`api/sse/parser.ts`), and the two had drifted
  already. The desktop copy commits the resume cursor at dispatch, as the
  WHATWG standard says. The mobile copy moved the cursor on an `id:` line of
  an event that was then cut off, so a resume could skip that event. The
  shared parser is the desktop one, so the mobile app gets the fix. The
  option is `maxEventChars` (the bound is on UTF-16 code units). The tests of
  both copies are merged here. The mobile test that replays its recorded
  streams stays in the app, because the fixtures are the app's.
- **The stream vocabulary.** The web app listed every frame type
  (`SocketMessageType`). The mobile app compared string literals. Both used
  the same event names, the same failure, delta and tool-call sets, and the
  same pause reasons. A new or renamed frame type now changes one list.
- **The brand defaults.** The mobile app kept a verbatim copy of the mark
  (with a parity test that read the web file) and hand-copied the default
  pack's hue and accents. The web app had three copies of the mark artwork
  (public, favicon, icon). All of them now derive from one source.

### Not shared, with the reason

- **Conversation delta sync (`changes_since`, tombstones, the sync cursor,
  page patching).** Only the mobile app syncs deltas. The web and desktop
  clients read the grouped folder listing and patch it in place
  (`removeRow`, cache invalidation): they have no cursor, no tombstones and
  no infinite pages. The mobile sync engine writes to its encrypted SQLite
  store in the same transaction as the cursor (`sync/engine.ts`,
  `db/repo.ts`), and its page patching works on TanStack `InfiniteData`.
  Neither part is pure or has a second consumer. Move the cursor and
  tombstone rules here when a second client syncs deltas, for example a
  desktop offline cache.
- **Turn-state reduction.** The reducers differ on purpose. The web reducer
  folds frames into the full message tree: sub-agents and their ownership,
  swarm hand-offs, reasoning, canvas, static pipeline pauses and node
  recovery. The mobile reducer folds one turn into a flat state for a phone
  thread: one answer, a capped tool list and one pause. The desktop Local
  work reducer (`features/workspace/model/turnReducer.ts`) folds a different
  channel, the host's `agent://event` stream. A shared reducer would force
  one view model on three different screens. The rules they agree on (the
  frame classes above) are shared.
- **Colour parsing and brand-pack mapping.** The web `color.ts` parses any
  CSS colour to re-hue the whole MUI palette. The mobile `theme/color.ts` is
  a strict allowlist, because it reads colours from a server pack into
  native styles. Their contracts are different.
- **Theme tokens.** The web palette is the MUI token set of the default pack
  (`default.pack.json`, `palette.augment.d.ts`). The mobile app has its own
  design system ("Elitea AI design tokens v1", `docs/design-tokens.json`),
  and reads only an allowlist of pack tokens (`TOKEN_MAP`). The default pack
  stays generated in the web app, and elitea-main pins its copy to it.
