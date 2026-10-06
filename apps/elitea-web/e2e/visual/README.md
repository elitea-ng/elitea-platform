# `e2e/visual/` — Playwright snapshot suite (`@visual`)

Runs in `ci-web-e2e.yml`'s `visual` job — every matching PR, pushes to `main`,
`v*` tags and manual dispatch — and only inside
`mcr.microsoft.com/playwright:v1.62.0-noble`.

## The rule that makes this suite worth having

**Never generate or update a baseline outside the pinned container.** Font
rasterisation and subpixel rendering differ between the container and any
developer host, so a host-generated baseline diffs against CI output on day one
— for reasons that have nothing to do with the UI. Issue #61 calls this "the
main thing that will make this suite useless if skipped".

The container MOUNTS this directory rather than installing into it, so
`node_modules` must already exist on the host — a fresh clone or a fresh git
worktree fails with `Cannot find package '@playwright/test'` before any test
runs. Run `npm ci` first. (The pinned image supplies the browsers, not the
packages, which is why this is easy to miss.)

```bash
# from apps/elitea-web, with the stack already up (up && seed)
npm ci                        # once per checkout — the container installs nothing
npm run e2e:visual            # compare against the committed baselines
npm run e2e:visual:update     # regenerate them
npm run e2e:visual -- --grep chat   # extra args pass through
```

`scripts/run-visual-container.mjs` is the whole of it: it derives the image tag
from `package.json`'s pinned `@playwright/test` — never writes it down — and
passes `E2E_REUSE_STACK=1`, which stops Playwright trying to start the stack
from inside a container that has no container runtime (see
`playwright.config.ts`). Because the tag is derived, bumping the library cannot
leave this command pointing at the old browser build;
`scripts/check-playwright-image-tag.mjs` asserts the same agreement for the
workflow files.

## Against a SECOND, parameterised stack

`deploy/docker-compose.e2e-standalone.yml` and `scripts/e2e-stack.sh` take
`E2E_PROJECT` / `E2E_PORT` / `E2E_PG_PORT` / `E2E_OIDC_PORT`
so a second stack can run beside the first (#228). To point this suite at it,
set `PLAYWRIGHT_BASE_URL` and `E2E_OIDC_PORT` — the script forwards those, plus
`E2E_TZ`, into the container:

```bash
E2E_PROJECT=elitea-e2e-b E2E_PORT=8086 E2E_PG_PORT=15434 \
  E2E_OIDC_PORT=9401 \
  ./scripts/e2e-stack.sh up && … seed

PLAYWRIGHT_BASE_URL=http://localhost:8086 E2E_OIDC_PORT=9401 npm run e2e:visual
```

Nothing inside the container can see the host's environment, so an unforwarded
variable is not a degraded run — it is a run against a DIFFERENT stack:
`playwright.config.ts` falls back to `http://localhost:8082` and
`auth.setup.ts` to OIDC port 9400. Both are unset by default, so CI and the
ordinary local path are unaffected.

`E2E_TZ` is forwarded for a stronger reason. It names the one IANA zone the
browser runs in AND the zone `e2e-stack.sh seed` anchors its calendar-day
fixtures in, so the two must always receive the same value (#214). Unforwarded,
setting it on the host would move the seed's day boundary and leave the browser
on UTC. It defaults to `UTC` on both sides.

## Start from a CLEAN stack

```bash
./scripts/e2e-stack.sh down -v && ./scripts/e2e-stack.sh up && ./scripts/e2e-stack.sh seed
```

Every baseline here encodes a freshly-seeded tenant: one project, no agents, no
pipelines, no buckets, no conversations. Running the journey suite first creates
`autotest_*` fixtures, and a baseline regenerated on top of them silently makes
those fixtures the reference. CI is safe by construction — `visual` and `e2e`
are separate jobs, each standing up its own stack — but a local `--update` after
a journey run is not.

**And rebuild the images.** `e2e-stack.sh up` runs `compose up -d --wait`, and
compose builds `ghcr.io/eliteaai/elitea-web:e2e` only when that tag is ABSENT
locally — so `down -v && up` after pulling or rebasing re-creates the containers
from the STALE image and the suite verifies code that is not in your tree. CI
does not have this problem (it runs two explicit `docker build`s before bringing
the stack up); a laptop does. From the repo root, not from `apps/elitea-web` —
the Containerfile paths are relative to the root and a wrong cwd fails the build
while leaving the previous image in place:

```bash
podman build --file services/elitea-main/Containerfile --target e2e \
  --tag ghcr.io/eliteaai/elitea-main:e2e --platform linux/amd64 .
podman build --file apps/elitea-web/Containerfile \
  --tag ghcr.io/eliteaai/elitea-web:e2e --platform linux/amd64 .
```

Check the image IDs actually changed afterwards. A cached rebuild can return a
byte-identical image, which is fine — a rebuild that silently did not happen is
not, and the two look the same from the exit code.

The failure is at least loud rather than silent, and that is deliberate: most
landmarks in `routes.visual.spec.ts` are resolved-empty-state copy ("You have no
agents.", "Still no conversations created."), so a polluted tenant fails the
landmark wait BEFORE any screenshot is taken. Measured, not hoped for: running
the suite against a tenant the journeys had just written to produced seven
`toBeVisible() failed … element(s) not found` errors and zero snapshot writes.

## Two spec files, two shells

`routes.visual.spec.ts` covers the MAIN app (`/app/**`) and
`admin.visual.spec.ts` covers the ADMIN SPA (`/admin/app/**`, issue #229).
They are separate files because they need different things, not for tidiness:

- **A different persona.** The `visual` project's default `storageState` is the
  member persona; every admin route is gated server-side on
  `administration`-mode permissions only the admin persona holds, so
  `admin.visual.spec.ts` sets `test.use({ storageState: STORAGE_STATE.admin })`.
  Without it every admin shot would be a photograph of a 403.
- **A different shell guard.** `shellSettled()` exists because the main app's
  sidebar is permission-FILTERED and renders shorter while
  `GET /auth/permissions/...` is in flight. The admin nav has no query behind it
  at all — it filters on `window.admin_ui_config.permissions`, which the Go
  handler substitutes into the HTML at request time — so `adminShellSettled()`
  asserts something else instead: that the SPA mounted and the rail is expanded
  (its collapsed flag is persisted in `localStorage`).

Shared helpers (`settle()`, the base mask list, the diff tolerance) live in
`lib/settle.ts`. They are NOT imported from `routes.visual.spec.ts`: importing a
`*.spec.ts` re-runs its `test()` registrations inside the importer, so every
route would be snapshotted twice under two names.

## Every landmark is proven, not chosen

A snapshot taken before a screen has finished rendering pins the loading state
as the reference, and then every future run matches it. That is not
hypothetical: it is what #159 found on `settings-analytics` (the committed
baseline was a photograph of a spinner) and what #174 found latent on two more
routes.

So no landmark is admitted on inspection. Each is measured with a stall
experiment — load the route with its own API stalled and check whether the
landmark still resolves — and the result is recorded next to it in
`routes.visual.spec.ts`. **Read that file's header before adding a spec**: it
documents the two ways the experiment gives a false pass (stalling the shell's
own endpoints disables page queries instead of pending them; a stall shorter
than the experiment lets the query fail into the same branch as success), both
of which produced wrong answers before they were caught.

`shellSettled()` guards the part of every shot that is not the route: the
sidebar's nav list is permission-filtered and renders SHORTER while
`GET /auth/permissions/...` is in flight, with no spinner to give it away, and
the project switcher renders the literal "No projects" until the project list
arrives.

## What these specs are, and are NOT

They answer **"has our rendering changed since the last commit"**. Baselines are
PNGs Playwright generated from our own app.

They are **not** compared against `parity/screenshot-index.json`'s 63 production
JPEGs, and cannot be: those are lossy JPEG, captured at a different DPR against a
real tenant, and they show the **old** app. Pixel-diffing against them would fail
on every screen by design.

The production screenshots are the **human** reference — you look at them while
writing a spec, to decide whether what we render is right. The Figma export
(`~/Documents/elitea/`, 1132 frames) is the design-intent reference. Three
artefacts, three questions; see that export's README.

## The tolerance is measured, not guessed

`lib/settle.ts`'s `SNAPSHOT_TOLERANCE` is `{ maxDiffPixels: 20, threshold: 0.05 }`.
Read the comment above it before changing either number — it records the
measurement, and the measurement is the only thing keeping the two failure modes
apart. Short version (issue #233):

- **The noise floor is ZERO pixels.** Renders of unchanged content inside the
  pinned container are byte-identical, on CI's ubuntu-latest amd64 runner and on
  a macOS host running the same image under emulation, across six full-suite
  runs including one against a stack rebuilt from an empty volume. The 0.002
  ratio this replaces was set to survive rasterisation noise that, measured,
  is not there.
- **`threshold` mattered more than the ratio.** pixelmatch discards a pixel
  whose YIQ delta is under `35215 * threshold²`, so at the default 0.2 a colour
  change needs a luminance step of ~53/255 to count at all. Recolouring every
  divider in the app scored exactly zero differing pixels at 0.2 — no
  `maxDiffPixels` value could have caught it.
- **The signal floor is 70 pixels**: the smallest deliberate change measured
  (swapping one 16px nav icon for its filled variant). A `Projects` →
  `Workspaces` nav rename is 384–418 px. The old budget was 2,716.

That gap is why this suite uses one global constant rather than per-shot
tolerances or component-scoped chrome shots: with 0 px of noise against a 70 px
signal there is nothing for either to buy.

**A baseline is generated ONLY in the pinned container** (above), and that rule
is now load-bearing rather than advisory: at `threshold: 0.05` a baseline
rasterised by anything else will not match.

## Coverage is deliberately partial

`scripts/check-visual-coverage.mjs` reports how many of the index's shots have a
spec, and fails only if a shot marked `wiringStatus: wired` has none.

Only `wired` routes are covered. Writing specs for routes still marked `ready`,
`needs-route-state`, `blocked-codegen` or `hybrid-defect` would bake stub UI into
a baseline and make it the official reference.

**`wiringStatus` is a claim about the ROUTE, not about the design.** It was
authored in 46a90914 with `route-wiring-map.json`'s Phase-0 vocabulary, when 38
route files rendered `RouteShell` scaffolding instead of the page components
already built for them — `ready` meant "the page exists, the route does not
render it". The wiring plan has since run, and the #61 review flipped fourteen
routes to `wired` on that basis plus a per-route check against the running
stack. See `screenshot-index.json`'s `wiringStatusReview` for what was flipped,
what was deliberately not, and why.

The admin routes were added to that index by #229, derived from
`src/pages/admin/router.tsx`'s own route table —
`src/pages/admin/adminVisualIndex.test.ts` asserts the two cannot drift, the way
`AdminNav.test.tsx` does for the nav. Before that change the index held zero
`/admin/app/*` rows, so the gate could not fail for an admin screen: #225 added
persistent navigation to all ten admin pages and altered zero snapshots, with
both `Visual regression` and `check-visual-coverage` green.

A route being `wired` is necessary but not sufficient: `/help-center`'s route
renders its real page and is marked `wired`, yet it is EXEMPT from snapshots
because `useResourcesConfig` has no backend (issue #26) and every card renders
"No links configured". A screen with no content is not the same as a screen with
empty content — `/artifacts` and the list pages render resolved, legitimately
empty data from endpoints that work, and their empty states are real UI.

When a route becomes `wired`, add its spec in the same change — with its stall
experiment.
