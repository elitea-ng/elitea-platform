/**
 * Helpers shared by every `@visual` spec.
 *
 * They live here rather than in `routes.visual.spec.ts` because a second spec
 * file (`admin.visual.spec.ts`) needs the identical ones, and importing them
 * from a `*.spec.ts` would re-run that file's `test()` registrations inside the
 * importer — every route snapshot would be taken twice, under two names.
 * `playwright.config.ts`'s `testMatch` is `/visual\/.+\.spec\.ts/`, so nothing
 * under `visual/lib/` is collected as a test.
 *
 * There is exactly one definition of `settle()` and one base mask list, which
 * is the point: two copies drifting apart would mean two suites claiming the
 * same guarantees while enforcing different ones.
 */
import { expect, type Locator, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

/**
 * Regions whose content is legitimately different on every run — timestamps,
 * relative dates, generated ids. Masked rather than asserted so the suite fails
 * on LAYOUT and STYLE changes only, which is what it is for. Masking is the
 * honest alternative to a high `maxDiffPixels`, which would hide real drift
 * everywhere instead of in the two places it is expected.
 */
export function volatileRegions(page: Page): Locator[] {
  return [
    page.locator('[data-testid$="-timestamp"]'),
    page.locator('time'),
    // The analytics date-range filter renders `now-24h … now` as formatted
    // wall-clock text (`DateRangeField`, format `dd/MM/yyyy HH:mm`). It is
    // different on every run by construction: the first observed run of this
    // job diffed a baseline reading 06/08/2026 23:21 against a page reading
    // 09/08/2026 00:51 (run 31345403013, issue #159). A locator that matches
    // nothing on the other routes is a no-op there.
    page.locator('[data-testid="analytics-date-range"]'),
  ];
}

/**
 * The MAIN app's shell landmark — every `/app/**` shot in this suite waits on
 * it before the shutter opens.
 *
 * Both halves are required: the nav link proves the permission query resolved
 * (and so the nav is at full length), the project name proves the project list
 * resolved (and so the switcher is not showing its "No projects" fallback).
 *
 * `Credentials` is chosen over the other gated items only because it is gated
 * and unambiguous; any of `PERMISSION_GROUPS`' entries would do. `Applications`
 * and `Skills` would NOT — `requiredPermissionsFor` returns undefined for them,
 * so they render during load too.
 *
 * MEASURED (shell-allowed stall vs. full stall, on
 * `/settings/create-personal-token`, which has no data of its own):
 *   Credentials nav link       loaded YES  stalled no   <- permission-gated
 *   resolved project name      loaded YES  stalled no
 *   "No projects" fallback     loaded no   stalled YES
 *   Applications nav link      loaded YES  stalled YES  <- ungated, no good
 *   sidebar-create-button      loaded YES  stalled YES  <- always present
 * The first two are the landmark; the last two are why the obvious choices are
 * not. See `../routes.visual.spec.ts`'s header for the full method note.
 *
 * It lives HERE, beside `settle()` and the base mask list, for the reason that
 * file's header gives for those: two spec files need the identical guarantee,
 * and a spec file cannot be imported from another spec file without re-running
 * its `test()` registrations. Two copies would be two suites claiming the same
 * guarantee while enforcing different ones.
 *
 * The ADMIN SPA does NOT use this — its nav is not query-driven, so it has a
 * different guard of its own (`adminShellSettled` in `../admin.visual.spec.ts`).
 */
export async function shellSettled(
  page: Page,
  projectName = 'Default Project',
): Promise<void> {
  await expect(page.getByRole('link', { name: 'Credentials', exact: true })).toBeVisible({
    timeout: 20_000,
  });
  // The selected project's NAME, not merely that the switcher exists: the
  // switcher renders either way, and its loading text is the literal
  // "No projects".
  //
  // PARAMETERISED, because the name was hardcoded to 'Default Project' with the
  // comment "the seeded tenant's only project" — true when written and no
  // longer. A route photographed with a different project selected (see
  // `VisualRoute.project`) would have waited twenty seconds for a name that was
  // never going to appear, and then failed on its landmark instead, which
  // reports the wrong cause.
  await expect(page.locator('button').filter({ hasText: projectName }).first()).toBeVisible({
    timeout: 20_000,
  });
}

/**
 * Animations frozen, images decoded, fonts loaded, caret hidden — the four
 * things that make two renders of an unchanged screen differ.
 *
 * ── WHY IMAGES ARE WAITED FOR, AND FONTS ARE NOT ENOUGH ────────────────────
 *
 * Playwright's own screenshot path already does two of these for us: the call
 * log of every shot in this suite reads "disabled all CSS animations" and
 * "waiting for fonts to load … fonts loaded". It does NOT wait for images, and
 * on this branch that is the gap that matters.
 *
 * The illustrated empty states (`shared/ui/EntityCardList/EntityEmptyState`)
 * paint `src/assets/empty-states/*.webp` at `width: 15rem; height: auto`, with
 * no width/height attributes and no `aspect-ratio`. The box is therefore ZERO
 * high until the WebP's header has arrived, and 170.65625px high afterwards
 * (720x512 source painted 240px wide). Everything under it — the heading, the
 * description, the `+ Create` pill — moves by 170px between those two states.
 * An un-waited image is not a small difference in such a shot; it is a
 * different screenshot, and `--update-snapshots` would pin whichever one the
 * shutter caught. `waitForTimeout(300)` was the only thing covering that, and
 * a fixed sleep is a hope, not a wait.
 *
 * SAID PLAINLY: this wait has not been caught doing work. On the standalone
 * stack, with the assets already in the HTTP cache, both `<img>` elements
 * reported `complete` at the landmark in 8 of 8 loads of
 * `/app/pipelines/latest`. It guards a cold cache and a slow runner, which is
 * where the 170px state lives, and it is not offered as a reproduction.
 *
 * ── WHAT THESE SHOTS ARE SENSITIVE TO, AND WHAT NO WAIT CAN FIX ────────────
 *
 * The illustration's height is not a whole pixel: 240 * 512 / 720 is 170.666…,
 * so every box under it sits off the pixel grid. Measured in the pinned image
 * against the standalone stack, on all 8 loads: the empty-state heading reports
 * `getBoundingClientRect().top = 361.40625` and the description 401.40625 —
 * device rows 722.8125 and 802.8125 at this project's `deviceScaleFactor: 2`.
 * Layout is REPRODUCIBLE (the same values every load); what is not guaranteed
 * is how a renderer rounds a glyph run onto a device row eight tenths of a
 * pixel away.
 *
 * Issue #819 is what that looks like when two runs round it differently. The
 * `pipelines-list-empty` failure is 1342 pixels, all of them 400-weight 14px
 * text — the two-line description and the rail's "No tags to display." The
 * illustration, the 600-weight heading and the `+ Create` pill are
 * byte-identical, so nothing MOVED; the same glyphs rasterised differently.
 * Two facts place the blame on the baseline rather than on the render: the
 * failing capture is byte-identical, over that text, to the baseline this one
 * REPLACED (846e55f0) and to the `agents-list-empty` baseline recorded in the
 * same regeneration run. One capture in that run disagreed with everything
 * before and after it, and it is the one that was committed.
 *
 * So a baseline recorded here can be an outlier, and no amount of waiting
 * prevents that. Two things do: regenerating in `ci-web-e2e.yml`'s
 * `full_visual_refresh` and reading `scripts/compare-visual-baselines.mjs`, and
 * — for the empty states specifically — giving the illustration an intrinsic
 * size so the text lands back on the grid. Both are outside this helper.
 *
 * ── WHY THE FONT WAIT IS WRITTEN OUT ───────────────────────────────────────
 *
 * `page.evaluate(() => document.fonts.ready)` handed Playwright's serialiser a
 * `FontFaceSet` to marshal back. `await`ing it inside the page and returning
 * nothing is the same wait without the round trip.
 *
 * It stays even though it is currently a no-op: measured on the E2E stack,
 * `document.fonts` reports `status: "loaded"`, `size: 0`, no faces at all —
 * the served pack is the product default, which declares no `typography.
 * fontFaces`, so `"Montserrat", Roboto, Arial, sans-serif` resolves to the
 * container's own sans. A deployment pack MAY declare self-hosted faces
 * (`shared/brand/fontFaces.ts`), and then this is the wait that keeps a
 * fallback-metrics layout out of a baseline.
 */
export async function settle(page: Page): Promise<void> {
  // Frozen FIRST, so anything that starts while the waits below run is already
  // static by the time it is asked to settle.
  await page.addStyleTag({
    content: `*, *::before, *::after {
      animation-duration: 0s !important;
      animation-delay: 0s !important;
      transition-duration: 0s !important;
      transition-delay: 0s !important;
      caret-color: transparent !important;
    }`,
  });
  // Every image has its intrinsic size, so nothing under it can still move.
  // A broken image is `complete` too, which is correct here: it will never
  // produce a size, and waiting for one would hang instead of photographing
  // the alt box the screen actually shows.
  await page.waitForFunction(
    () => Array.from(document.images).every((image) => image.complete),
    undefined,
    { timeout: 20_000 },
  );
  // Decoded as well as loaded: `complete` settles LAYOUT, `decode()` settles
  // the PIXELS, and a first paint of an undecoded image is blank.
  await page.evaluate(async () => {
    await Promise.all(
      Array.from(document.images).map((image) => image.decode().catch(() => undefined)),
    );
    await document.fonts.ready;
  });
  // Two frames: the frozen styles above, the decoded images and the loaded
  // fonts have all been through one full lifecycle before the shutter.
  await page.evaluate(
    async () =>
      new Promise<void>((resolve) => {
        requestAnimationFrame(() => {
          requestAnimationFrame(() => {
            resolve();
          });
        });
      }),
  );
  await page.waitForTimeout(300);
}

/**
 * The comparison options every shot in this suite uses.
 *
 * ── THE MEASURED NOISE FLOOR IS ZERO PIXELS (issue #233) ───────────────────
 *
 * This used to read `{ maxDiffPixelRatio: 0.002 }`, chosen under #159 on the
 * reasoning that font rasterisation and antialiasing differ enough between
 * environments that a zero tolerance produces constant false failures. Nobody
 * had measured how much noise there actually is, so nobody could see what else
 * that number was absorbing: renaming the admin nav's `Projects` label to
 * `Workspaces` — user-visible text in persistent chrome on all ten admin pages
 * — failed NO baseline.
 *
 * So it was measured, at `{ maxDiffPixels: 0 }`, over six full-suite runs in
 * the pinned container (39 shots each) in TWO environments — this repo's CI
 * (ubuntu-latest, amd64, native) and a macOS host running the same image under
 * podman with amd64 emulation — including a stack destroyed and rebuilt from an
 * empty volume, and a bundle rebuilt by a different toolchain (node 26 on the
 * host vs node 24 in the builder image):
 *
 *   threshold   worst per-shot noise across those runs
 *   ---------   --------------------------------------
 *   0.2                       0 px          (pixelmatch's default)
 *   0.1                       0 px
 *   0.05                      1 px          ← what this file now uses
 *   0.02                      9 px
 *   0.01                     20 px
 *   0                        28 px
 *
 * Not "small": renders of unchanged content are BYTE-IDENTICAL down to
 * threshold 0.1, on both machines, every run. There is no rasterisation noise
 * to protect against inside the pinned container, only the sub-threshold
 * glyph-edge jitter the last three rows show.
 *
 * ── WHY `threshold` MOVED, AND WHY IT MATTERED MORE THAN THE RATIO ─────────
 *
 * Playwright compares with pixelmatch, whose `threshold` decides whether a
 * pixel counts as different at all: `maxDelta = 35215 * threshold²` in YIQ
 * space, so the default 0.2 needs a luminance step of roughly 53/255 before ONE
 * pixel is counted. Measured on this suite: changing the dark scheme's
 * `border.lines` from #3B3E46 to #545862 — every divider, card edge and input
 * underline in the app — produced exactly ZERO differing pixels on all 39 shots
 * at threshold 0.2 AND at 0.1. No value of `maxDiffPixels` or
 * `maxDiffPixelRatio` can catch a change the comparator has already discarded.
 * At 0.05 the same change is 1,033–3,615 px per shot.
 *
 * 0.05 is therefore the operating point: the last row above where noise is
 * ~1 px, and the first where a colour-only regression is visible at all.
 *
 * ── THE SIGNAL FLOOR IS 70 PIXELS ──────────────────────────────────────────
 *
 * Five minimal changes, each built and served to the running stack, measured at
 * threshold 0.05 (page shots are 1602x848 = 1,358,496 px, so the OLD 0.002
 * allowed 2,716):
 *
 *   change                                       px/shot   old 0.002 verdict
 *   ------------------------------------------   -------   -----------------
 *   admin nav icon, outlined → filled folder      70–81    passed (38x under)
 *   nav label `Projects` → `Workspaces`          384–418   passed (6.5x under)
 *   admin nav border-right 1px → 2px             261–325   passed (8x under)
 *   `border.lines` #3B3E46 → #545862           1033–3615   passed (invisible)
 *   the accent-hue recolour found below           586–750   passed for months
 *
 * `maxDiffPixels` is an absolute count, not a ratio, because the noise it
 * covers is a handful of glyph-edge pixels and does not scale with image area —
 * a ratio makes a large page shot 7x more forgiving than a small one for no
 * measured reason.
 *
 * ── THE 70 PX SIGNAL FLOOR WAS WRONG, AND 20 WAS ABOVE A REAL SIGNAL ───────
 *
 * The five changes above were all measured in the DARK scheme. The same change
 * moves a different number of pixels in each scheme, because the count depends
 * on the contrast between the changed pixels and what they replaced — and the
 * budget was set from the dark numbers alone.
 *
 * Measured on issue #238's sidebar chevron, a 16x16 dropdown arrow added to
 * `ProjectSwitcher`, which is in every route shot. Counted with Playwright's
 * own comparator (`getComparator('image/png')`) at this file's threshold, on
 * all 7 routes that carry both schemes:
 *
 *   scheme   px/shot   verdict at maxDiffPixels: 20
 *   ------   -------   ----------------------------
 *   dark        21     failed on all 7 — by ONE pixel
 *   light       11     PASSED on all 7
 *
 * So the smallest real signal is 11 px, not 70. The budget of 20 sat ABOVE it,
 * which inverts the rule this file set for itself: a budget must sit below the
 * smallest change worth catching. The light half of the suite went on passing
 * against baselines with no chevron in them, and `--update-snapshots=changed`
 * never rewrote them, because a passing snapshot is not rewritten. Those
 * baselines had to be DELETED to be regenerated. The dark half was caught by a
 * one-pixel margin, which is luck, not a working gate.
 *
 * The noise floor was re-measured at the same time, the same way: 19
 * unchanged-content shots compared across two independent full-suite runs in
 * CI, at this threshold — min 0, max 0, mean 0 differing pixels. That agrees
 * with the 0–1 px above and is why the budget can be small.
 *
 * `maxDiffPixels: 3` therefore sits 3x above the worst noise ever observed
 * (1 px; 0 px in the runs just described) and 3.7x below the smallest real
 * signal now known (11 px) — the same shape of margin 20 was chosen for, with
 * the signal floor corrected. Anything measured below 11 px in future belongs
 * in the table above, and the budget moves with it.
 *
 * ── THIS WAS NOT HYPOTHETICAL ──────────────────────────────────────────────
 *
 * The first zero-tolerance run found three baselines
 * (`settings-personalization`, `settings-create-personal-token`,
 * `settings-project-params`) still carrying the app's OLD teal accent hue,
 * dating from f82ea3a6 (#82) while every baseline regenerated since carries the
 * magenta one. The whole of the persistent chrome — the CREATE button, the
 * ELITEA mark, the Agent HUB item — was recoloured, and it scored 586/750 px
 * against a 2,716 px budget, so the gate stayed green across every PR in
 * between. Both environments reproduced those counts to the pixel, which is
 * what proves it was a real difference rather than noise. Those three baselines
 * were regenerated in the pinned container as part of #233.
 *
 * ── WHAT WAS CONSIDERED AND REJECTED ───────────────────────────────────────
 *
 * PER-SHOT tolerances: nothing to differentiate. The noise floor is 0–1 px on
 * every shot regardless of its size or how busy it is, so a large dashboard and
 * a small dialog need the same budget.
 *
 * COMPONENT-SCOPED chrome shots (snapshot the nav, not the page) to make a
 * label change a large fraction of a small image: still not needed, but the
 * reason has shrunk. The gap between signal and noise is 11x, not the 70x this
 * note used to claim, and the amplification would be ~7x. It is now a real
 * option if a change smaller than 11 px ever has to be caught, rather than the
 * obviously-unnecessary one it reads as.
 *
 * ── WHAT A TIGHTER BUDGET DOES NOT FIX ─────────────────────────────────────
 *
 * A snapshot that PASSES is never rewritten, whatever the budget is. So a
 * baseline that drifted under the old budget stays wrong until someone deletes
 * it, and a stale baseline is indistinguishable from a correct one by looking
 * at a green run. Lowering the number shrinks the window; it does not close it.
 * `scripts/compare-visual-baselines.mjs` and the `full` regeneration mode in
 * `ci-web-e2e.yml` exist for that: a full rewrite, compared by PIXELS rather
 * than by bytes, so accumulated drift is visible in one reviewable list.
 *
 * ── AND `threshold` HIDES A CHANGE THE BUDGET NEVER SEES ───────────────────
 *
 * The note above is about `maxDiffPixels` — too FEW pixels differing. The
 * other half is `threshold`, which decides whether a pixel differs AT ALL, and
 * it can zero out a change of any size. pixelmatch scores a pair of colours on
 * a YIQ metric and calls them equal below `35215 * threshold ** 2`; at 0.05
 * that ceiling is 88.0.
 *
 * The worked example is the DeepWiki wiki list. `.Mui-selected` on a
 * `ListItemButton` is `alpha(primary.main, action.selectedOpacity)` — 16% cyan
 * in dark, 8% magenta in light. Highlighting the open wiki moved both shots by
 * ~80,000 pixels. Dark scored 396 per pixel and failed loudly. Light went from
 * #FFFFFF to #FAEEFC and scored 81.9 — six percent under the ceiling — so ZERO
 * pixels counted, `deepwiki-browser-light` passed, and
 * `--update-snapshots=changed` left the pre-change baseline in place. The
 * screen it documents no longer exists, and no budget would have caught it:
 * the count was already 0. A full refresh (`full_visual_refresh`) is the only
 * mode that rewrites it, and even then `compare-visual-baselines.mjs` reports
 * it as "unchanged (re-encoded only)", because that script scores the same way.
 * The tint was confirmed to render by reading `getComputedStyle` in a real
 * Chromium, not inferred from the snapshot.
 *
 * So: a light-scheme state that is drawn only as a low-alpha wash is outside
 * what this suite can assert. Pin it in a unit or Storybook test instead of
 * assuming a green visual run covers it.
 *
 * `pipeline-editor-authored-graph` was the same trap wearing different
 * clothes, and it is the reason a full refresh is worth running even when
 * nothing looks wrong. Its four siblings were refreshed at #739; it was not,
 * because its diff scored under the ceiling, so it sat at its #615 rendering
 * while they moved on. The whole 17,640 px is the React Flow dot lattice
 * sitting one pixel to the right: the node cards do not move at all (a card
 * edge shifting a pixel would score in the hundreds, and the maximum here is
 * 42.9), because `<Background>` phases its `<pattern>` on `x % gap` and rounds
 * separately from the viewport's own CSS transform.
 *
 * ── TELLING DRIFT FROM FLAKE ───────────────────────────────────────────────
 *
 * Before adopting any sub-threshold refresh, run the full refresh TWICE and
 * diff the two artifacts against each other. Measured on this branch: the
 * noise floor between two runs is 11-70 px on 17 of the 50 shots, so a shot
 * that reproduces its diff EXACTLY across both runs is recording a real
 * change, and one that differs run to run is recording the renderer. Both
 * baselines taken here cleared it — 17,640 px and 80,152 px against a 27 px
 * and 0 px run-to-run delta respectively.
 */
export const SNAPSHOT_TOLERANCE = { maxDiffPixels: 3, threshold: 0.05 } as const;

/** A project a shot must be taken in. See `VisualRoute.project` in `routes.visual.spec.ts`. */
export interface VisualProject {
  readonly id: string;
  readonly name: string;
}

/**
 * Select `project` THROUGH THE SWITCHER, the way a user does.
 *
 * The first version wrote `el.project.id` with `addInitScript` before the first
 * navigation. It works in a journey and it did NOT work here: the run came back
 * with the switcher still on Default Project and the landmark missing, so the
 * seeded wiki's project was never selected and the shot would have photographed
 * a project with no wiki. Whatever the interaction with the restored
 * `storageState` is, writing another test's storage internals is a mechanism
 * this suite has no way to notice breaking.
 *
 * Clicking is slower and it is the product's own path: the switcher is the only
 * supported way to change project, J7 covers it end to end, and the selection
 * persists to both storage areas by the app's own code rather than by ours.
 *
 * Returns the name the shell should then settle on, so the caller cannot wait
 * for one project while having selected another. `undefined` selects nothing
 * and returns the seeded default's name.
 *
 * Shared for the same reason `settle()` is: `routes.visual.spec.ts` and
 * `brand.visual.spec.ts` (ADR-0024 WP6) both photograph the seeded wiki.
 */
export async function selectProject(page: Page, project: VisualProject | undefined): Promise<string> {
  if (!project) return 'Default Project';

  await page.goto(BASE_URL + '/app/', { waitUntil: 'domcontentloaded' });
  await shellSettled(page);

  const trigger = page.getByRole('button', { name: /Project:/ });
  await expect(trigger).toBeVisible({ timeout: 20_000 });
  await trigger.click();

  const listbox = page.getByRole('listbox');
  await expect(listbox).toBeVisible({ timeout: 10_000 });
  await listbox.getByRole('option', { name: project.name }).click();

  await expect(trigger).toHaveAccessibleName(new RegExp(`Project:\\s*${project.name}`), {
    timeout: 20_000,
  });
  return project.name;
}
