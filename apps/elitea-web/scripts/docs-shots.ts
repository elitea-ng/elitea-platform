#!/usr/bin/env npx tsx
/**
 * docs-shots.ts — the repeatable screenshot capture driver for the embedded
 * docs SPA (embedded-docs programme, unit W3, PREAMBLE decision 4).
 *
 * Reads `src/entries/docs/shots.manifest.ts`, signs in to a RUNNING e2e
 * stack (`apps/elitea-web/scripts/e2e-stack.sh up && … seed`) the same way
 * the Playwright suite does, and for every manifest entry: opens the route,
 * lets it settle deterministically, applies any masks, captures a PNG, and
 * re-encodes it to WebP under `content/img/<id>.webp` at ≤250 KB.
 *
 * ## Why this reuses `playwright.config.ts` rather than re-implementing sign-in
 *
 * `e2e/auth.setup.ts` drives a real OIDC round trip once per persona and
 * writes the result to `STORAGE_STATE.{member,admin,chat}` (both exported by
 * `playwright.config.ts`). Those files ARE the "signed in as this persona"
 * contract every journey and every visual baseline already trusts — a
 * screenshot driver that logged in a second, different way would prove
 * nothing about whether the pages it photographs are the ones a real user
 * reaches. So this script does not touch OIDC at all: it opens a browser
 * context with `storageState: STORAGE_STATE[persona]`, and if that file does
 * not exist yet (a stack that was brought up and seeded but never had
 * `npx playwright test --project=setup` run against it), it runs exactly
 * that project once, which is the same thing CI's `e2e` job and a local
 * `npx playwright test` both already do before any journey starts.
 *
 * ## Determinism
 *
 *  - `reducedMotion: 'reduce'` (context option) plus an injected stylesheet
 *    that zeroes every animation/transition duration and hides the caret —
 *    the same two mechanisms `e2e/visual/lib/settle.ts` uses for the @visual
 *    suite, applied here for the same reason.
 *  - Light theme through the app's OWN toggle
 *    (`/app/settings/personalization`, the `Light`/`Dark` buttons), not by
 *    writing `localStorage['el-mode']` — `e2e/visual/routes.visual.spec.ts`'s
 *    `useLightScheme()` explains why: issue #61 is explicit that a
 *    light-scheme shot must exercise the real control, not fake the
 *    stylesheet's attribute out from under it.
 *  - No `page.waitForTimeout`. "Network idle" is Playwright's own
 *    `waitForLoadState('networkidle')`; "stable DOM" is a `MutationObserver`
 *    quiet-window (`waitForDomStable` below) — a real signal, not a blind
 *    sleep, and it resolves the moment the page stops mutating rather than
 *    after a fixed guess.
 *  - A fixed clock is NOT wired in generically. `e2e/journeys/admin/
 *    admin.audit-trail.spec.ts` is the one place in this suite that needs
 *    one (`page.clock.setFixedTime`, anchored to `AUDIT_FIXTURE_ANCHOR`), and
 *    no current manifest entry photographs that page. A future shot that
 *    does should add a `theme`-shaped opt-in here rather than freezing every
 *    shot's clock for a need only one of them has.
 *
 * ## Masking
 *
 * `mask` selectors are blanked with a solid, positioned overlay rather than
 * Playwright's own `mask` option (`toHaveScreenshot`'s pink boxes): these
 * PNGs are committed documentation images, not diff baselines, and a pink
 * rectangle reads as a bug report in a screenshot meant to explain the
 * product.
 *
 * ## CLI
 *
 *   npx tsx scripts/docs-shots.ts [--only id,id,…] [--base-url URL]
 *                                 [--persona member|admin|chat]
 *                                 [--fail-on-missing] [--seed-map PATH]
 *                                 [--report DIR]
 *
 *   --only              Comma-separated shot ids to capture (default: all).
 *   --base-url          Overrides playwright.config.ts's BASE_URL /
 *                       PLAYWRIGHT_BASE_URL.
 *   --persona           Default persona for shots that do not name one
 *                       (default: member).
 *   --seed-map          Path to docs-seed.ts's JSON output, used to resolve
 *                       a route's `:placeholder` segments (default:
 *                       playwright-results/docs-seed.json). A route with no
 *                       placeholder works with no seed map at all.
 *   --report            Also write a PNG copy of every attempted capture
 *                       (bad ones included) to this directory, named
 *                       `<id>.png`, for a human review pass — separate from
 *                       the committed `content/img/<id>.webp`.
 *   --fail-on-missing   Exit non-zero if any requested shot produced no
 *                       `content/img/<id>.webp` (capture error, a route
 *                       whose placeholder did not resolve, a detected bad
 *                       capture — see `detectBadCapture` — or a
 *                       conversion that could not reach the byte ceiling).
 */
import { execFileSync } from "node:child_process";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import sharp from "sharp";
import {
  chromium,
  type Browser,
  type BrowserContext,
  type Page,
} from "playwright";

import { BASE_URL, STORAGE_STATE } from "../playwright.config";
import type {
  Shot,
  ShotAction,
  ShotPersona,
} from "../src/entries/docs/shots.manifest";
import { shots as manifestShots } from "../src/entries/docs/shots.manifest";

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const WEB_ROOT = resolve(SCRIPT_DIR, "..");
const IMG_DIR = join(WEB_ROOT, "src/entries/docs/content/img");
const MAX_IMAGE_BYTES = 250 * 1024;
const MIN_WEBP_QUALITY = 30;
const RESIZE_WIDTH = 1200;

const CHROMIUM_LAUNCH_OPTIONS = {
  args: [
    "--disable-web-security",
    "--allow-insecure-localhost",
    "--no-sandbox",
  ],
};

interface Cli {
  readonly only: readonly string[] | undefined;
  readonly baseUrl: string;
  readonly persona: ShotPersona;
  readonly failOnMissing: boolean;
  readonly seedMapPath: string;
  readonly reportDir: string | undefined;
}

/** Default `docs-seed.ts` output path, mirrored here so the two scripts agree
 * on a default without one importing the other. */
const DEFAULT_SEED_MAP_PATH = join(WEB_ROOT, "playwright-results/docs-seed.json");

function parseArgs(argv: readonly string[]): Cli {
  let only: string[] | undefined;
  let baseUrl = BASE_URL;
  let persona: ShotPersona = "member";
  let failOnMissing = false;
  let seedMapPath = DEFAULT_SEED_MAP_PATH;
  let reportDir: string | undefined;

  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    switch (arg) {
      case "--only": {
        const value = argv[++i];
        if (value === undefined) throw new Error("--only requires a value");
        only = value
          .split(",")
          .map((id) => id.trim())
          .filter((id) => id !== "");
        break;
      }
      case "--base-url": {
        const value = argv[++i];
        if (value === undefined) throw new Error("--base-url requires a value");
        baseUrl = value.replace(/\/+$/, "");
        break;
      }
      case "--persona": {
        const value = argv[++i];
        if (value !== "member" && value !== "admin" && value !== "chat") {
          throw new Error(
            `--persona must be one of member|admin|chat, got: ${String(value)}`,
          );
        }
        persona = value;
        break;
      }
      case "--fail-on-missing":
        failOnMissing = true;
        break;
      case "--seed-map": {
        const value = argv[++i];
        if (value === undefined) throw new Error("--seed-map requires a value");
        seedMapPath = resolve(value);
        break;
      }
      case "--report": {
        const value = argv[++i];
        if (value === undefined) throw new Error("--report requires a value");
        reportDir = resolve(value);
        break;
      }
      default:
        throw new Error(`unknown argument: ${arg}`);
    }
  }
  return { only, baseUrl, persona, failOnMissing, seedMapPath, reportDir };
}

/**
 * Reads `docs-seed.ts`'s output map, or `{}` if it does not exist yet — a
 * manifest entry with no placeholder in its route works fine with an empty
 * map, and this script should not force every caller to have run the seed
 * first (e.g. a re-capture of one placeholder-free `--only` id).
 */
function readSeedMap(path: string): Record<string, unknown> {
  if (!existsSync(path)) {
    console.warn(
      `docs-shots: no seed map at ${path} — routes with a ":placeholder" will fail to resolve. ` +
        "Run docs-seed.ts first if any requested shot needs one.",
    );
    return {};
  }
  return JSON.parse(readFileSync(path, "utf8")) as Record<string, unknown>;
}

/**
 * Resolves every `:name` segment in `route` from `seedMap`, honouring the
 * shot's own `placeholders` override (see shots.manifest.ts's doc on that
 * field) before falling back to `seedMap[name]` directly. Throws, naming the
 * unresolved segment, rather than navigating to a route that still contains
 * a literal `:agentId` — which would "succeed" as a capture of a 404 page.
 */
function resolveRoute(shot: Shot, seedMap: Record<string, unknown>): string {
  return shot.route.replace(/:([A-Za-z][A-Za-z0-9_]*)/g, (match, name: string) => {
    const seedMapKey = shot.placeholders?.[name] ?? name;
    const value = seedMap[seedMapKey];
    if (typeof value !== "string" && typeof value !== "number") {
      throw new Error(
        `route placeholder "${match}" (seed map key "${seedMapKey}") did not resolve to a ` +
          `string/number in the seed map — got ${JSON.stringify(value)}. Run docs-seed.ts, or ` +
          "check the shot's `placeholders` override.",
      );
    }
    return String(value);
  });
}

/**
 * `STORAGE_STATE` is written by `e2e/auth.setup.ts`'s `setup` project — all
 * three personas in one sequential run (they compete for one server-side
 * personal-project provisioning slot; see that file's header). If none of
 * the files a run needs exist yet, run that project once rather than
 * re-deriving the login flow here.
 */
function ensureStorageStates(
  personas: ReadonlySet<ShotPersona>,
  baseUrl: string,
): void {
  const missing = Array.from(personas).filter(
    (persona) => !existsSync(STORAGE_STATE[persona]),
  );
  if (missing.length === 0) return;
  console.log(
    `docs-shots: missing storageState for ${missing.join(", ")} — running ` +
      `"npx playwright test --project=setup" against ${baseUrl} first.`,
  );
  execFileSync("npx", ["playwright", "test", "--project=setup"], {
    cwd: WEB_ROOT,
    stdio: "inherit",
    env: { ...process.env, PLAYWRIGHT_BASE_URL: baseUrl, E2E_REUSE_STACK: "1" },
  });
  for (const persona of missing) {
    if (!existsSync(STORAGE_STATE[persona])) {
      throw new Error(
        `docs-shots: setup ran but ${STORAGE_STATE[persona]} still does not exist ` +
          `(persona: ${persona}). Check the setup project's own output above.`,
      );
    }
  }
}

/** The animation/transition/caret kill `e2e/visual/lib/settle.ts` applies,
 * duplicated here rather than imported: that module lives under `e2e/`,
 * which `playwright.config.ts`'s `testMatch` treats as spec-adjacent, not a
 * library this script should reach into. */
const FREEZE_CSS = `*, *::before, *::after {
  animation-duration: 0s !important;
  animation-delay: 0s !important;
  transition-duration: 0s !important;
  transition-delay: 0s !important;
  caret-color: transparent !important;
}`;

/**
 * Resolves once the page has gone quiet: no DOM mutation for `quietMs`, or
 * `timeoutMs` has elapsed regardless (a page that legitimately never stops
 * — a live clock, a polling widget — must not hang the capture forever).
 * This is the "stable DOM" half of the wait; `waitForLoadState('networkidle')`
 * is the network half, run by the caller first.
 */
async function waitForDomStable(
  page: Page,
  quietMs = 500,
  timeoutMs = 8_000,
): Promise<void> {
  // No named intermediate (`const finish = () => …`) inside this closure:
  // `tsx`'s esbuild transform runs with `keepNames: true` and wraps every
  // NAMED function/arrow binding in `__name(fn, "fn")`, referencing a helper
  // defined once at the top of the whole transpiled module. `page.evaluate`
  // ships only this callback's OWN `.toString()`'d source to the browser, so
  // a wrapped inner binding turns into a `ReferenceError: __name is not
  // defined` there — reproduced and confirmed against this exact tsx version
  // before writing it this way. Every function below is passed anonymously,
  // straight into `setTimeout`/`MutationObserver`, which esbuild does not
  // wrap (there is no variable name to preserve).
  await page.evaluate(
    ({ quietMs, timeoutMs }) =>
      new Promise<void>((resolve) => {
        let quietTimer: ReturnType<typeof setTimeout>;
        const observer = new MutationObserver(() => {
          clearTimeout(quietTimer);
          quietTimer = setTimeout(() => {
            clearTimeout(ceiling);
            observer.disconnect();
            resolve();
          }, quietMs);
        });
        observer.observe(document.documentElement, {
          childList: true,
          subtree: true,
          attributes: true,
          characterData: true,
        });
        quietTimer = setTimeout(() => {
          clearTimeout(ceiling);
          observer.disconnect();
          resolve();
        }, quietMs);
        const ceiling = setTimeout(() => {
          observer.disconnect();
          resolve();
        }, timeoutMs);
      }),
    { quietMs, timeoutMs },
  );
}

/** Every image decoded, every font loaded — the two things a MutationObserver
 * cannot see because neither one mutates the DOM. */
async function waitForMedia(page: Page): Promise<void> {
  await page.waitForFunction(
    () => Array.from(document.images).every((img) => img.complete),
    undefined,
    {
      timeout: 20_000,
    },
  );
  await page.evaluate(async () => {
    await Promise.all(
      Array.from(document.images).map((img) =>
        img.decode().catch(() => undefined),
      ),
    );
    await document.fonts.ready;
  });
}

/**
 * Switches the app's OWN colour scheme through
 * `/app/settings/personalization`'s toggle, the same real control
 * `e2e/visual/routes.visual.spec.ts`'s `useLightScheme()` drives, and
 * confirms `data-el-scheme` actually changed before returning.
 */
async function setColorScheme(
  page: Page,
  baseUrl: string,
  theme: "light" | "dark",
): Promise<void> {
  const label = theme === "light" ? "Light" : "Dark";
  await page.goto(`${baseUrl}/app/settings/personalization`, {
    waitUntil: "domcontentloaded",
  });
  const button = page.getByRole("button", { name: label, exact: true });
  await button.waitFor({ state: "visible", timeout: 20_000 });
  await button.click();
  // `waitForFunction` polls the predicate itself (no fixed sleep to tune);
  // it is the "real signal" equivalent of the retry loop this replaced.
  await page.waitForFunction(
    (want) => document.documentElement.getAttribute("data-el-scheme") === want,
    theme,
    { timeout: 10_000 },
  );
}

/** Runs one manifest `actions` step. Kept intentionally small: the free
 * `type` string is the extension point (manifest.ts's own comment), and an
 * unrecognised one is a hard failure rather than a silent no-op. */
async function runAction(page: Page, action: ShotAction): Promise<void> {
  const locator =
    action.selector !== "" ? page.locator(action.selector) : undefined;
  switch (action.type) {
    case "click":
      await (locator ?? page.locator("body")).click();
      return;
    case "hover":
      await (locator ?? page.locator("body")).hover();
      return;
    case "fill":
      if (locator === undefined)
        throw new Error("fill action requires a selector");
      await locator.fill(action.value ?? "");
      return;
    case "type":
      if (locator === undefined)
        throw new Error("type action requires a selector");
      await locator.pressSequentially(action.value ?? "");
      return;
    case "press":
      if (locator !== undefined) await locator.press(action.value ?? "Enter");
      else await page.keyboard.press(action.value ?? "Enter");
      return;
    case "wait-for":
      if (locator === undefined)
        throw new Error("wait-for action requires a selector");
      await locator.waitFor({
        state:
          (action.value as
            "visible" | "hidden" | "attached" | "detached" | undefined) ??
          "visible",
        timeout: 20_000,
      });
      return;
    default:
      throw new Error(`unknown action type: ${action.type}`);
  }
}

/**
 * Blanks every `mask` selector with a solid, absolutely-positioned overlay
 * instead of Playwright's own pink `mask` boxes (see the file header).
 */
async function applyMask(
  page: Page,
  selectors: readonly string[],
): Promise<void> {
  if (selectors.length === 0) return;
  await page.evaluate((selectors) => {
    for (const selector of selectors) {
      const elements = document.querySelectorAll(selector);
      elements.forEach((el) => {
        const rect = el.getBoundingClientRect();
        const overlay = document.createElement("div");
        overlay.setAttribute("data-docs-shots-mask", "");
        overlay.style.position = "absolute";
        overlay.style.left = `${rect.left + window.scrollX}px`;
        overlay.style.top = `${rect.top + window.scrollY}px`;
        overlay.style.width = `${rect.width}px`;
        overlay.style.height = `${rect.height}px`;
        overlay.style.background = "#c7cbd1";
        overlay.style.zIndex = "2147483647";
        overlay.style.pointerEvents = "none";
        document.body.appendChild(overlay);
      });
    }
  }, selectors);
}

interface CaptureResult {
  readonly id: string;
  readonly route: string;
  readonly bytes: number | undefined;
  readonly error: string | undefined;
}

/**
 * Detects an obviously bad capture: a 404/error page, or the app's own error
 * boundary — checked BEFORE the shutter, so a page that failed to load never
 * becomes a committed screenshot of a crash. Title/URL are checked for the
 * literal substrings "404" and "error" (case-insensitive); the DOM is
 * checked for `[data-testid="error-boundary"]` and the text "Something went
 * wrong", the two surfaces this app's own error boundaries render.
 */
async function detectBadCapture(page: Page): Promise<string | undefined> {
  const title = await page.title().catch(() => "");
  const url = page.url();
  const badTextPattern = /404|error/i;
  if (badTextPattern.test(title)) {
    return `page title looks like an error page: "${title}"`;
  }
  if (badTextPattern.test(new URL(url).pathname)) {
    return `URL path looks like an error route: "${url}"`;
  }
  const errorBoundary = page.locator('[data-testid="error-boundary"]');
  if ((await errorBoundary.count()) > 0) {
    return 'found [data-testid="error-boundary"] on the page';
  }
  const somethingWrong = page.getByText(/Something went wrong/i);
  if ((await somethingWrong.count()) > 0) {
    return 'found "Something went wrong" text on the page';
  }
  return undefined;
}

/** Encodes `png` to WebP under `MAX_IMAGE_BYTES`, stepping quality down and
 * then, if still over budget, resizing to `RESIZE_WIDTH` wide. Returns the
 * final buffer even if the ceiling could not be reached, so the caller can
 * still write SOMETHING and report the true size rather than silently
 * dropping the shot. */
async function encodeWebp(png: Buffer): Promise<Buffer> {
  const qualities = [82, 72, 62, 52, 42, MIN_WEBP_QUALITY];
  let best: Buffer | undefined;
  for (const quality of qualities) {
    const candidate = await sharp(png).webp({ quality }).toBuffer();
    best = candidate;
    if (candidate.byteLength <= MAX_IMAGE_BYTES) return candidate;
  }
  // Still over budget at the lowest quality — scale down and try again at a
  // moderate quality, which recovers far more bytes than another quality step.
  const resized = await sharp(png)
    .resize({ width: RESIZE_WIDTH })
    .webp({ quality: 62 })
    .toBuffer();
  if (resized.byteLength <= MAX_IMAGE_BYTES) return resized;
  const resizedLow = await sharp(png)
    .resize({ width: RESIZE_WIDTH })
    .webp({ quality: MIN_WEBP_QUALITY })
    .toBuffer();
  if (resizedLow.byteLength <= MAX_IMAGE_BYTES) return resizedLow;
  return best ?? resizedLow;
}

async function captureShot(
  browser: Browser,
  shot: Shot,
  baseUrl: string,
  cliPersona: ShotPersona,
  tmpDir: string,
  seedMap: Record<string, unknown>,
  reportDir: string | undefined,
): Promise<CaptureResult> {
  const persona = shot.persona ?? cliPersona;
  const theme = shot.theme ?? "light";
  const route = resolveRoute(shot, seedMap);
  let context: BrowserContext | undefined;
  try {
    context = await browser.newContext({
      storageState: STORAGE_STATE[persona],
      viewport: shot.viewport,
      baseURL: baseUrl,
      reducedMotion: "reduce",
    });
    const page = await context.newPage();

    await setColorScheme(page, baseUrl, theme);

    await page.goto(`${baseUrl}${route}`, {
      waitUntil: "domcontentloaded",
    });
    await page.addStyleTag({ content: FREEZE_CSS });
    await page
      .waitForLoadState("networkidle", { timeout: 20_000 })
      .catch(() => {
        // Some routes hold a long-lived stream (chat, notifications) that
        // never goes idle by design; the DOM-stability wait below is what
        // actually gates the shutter.
      });
    await waitForDomStable(page);
    await waitForMedia(page);

    for (const action of shot.actions ?? []) {
      await runAction(page, action);
      await waitForDomStable(page);
    }

    const badCapture = await detectBadCapture(page);
    if (badCapture !== undefined) {
      throw new Error(`bad capture detected before the shutter: ${badCapture}`);
    }

    await applyMask(page, shot.mask ?? []);

    const pngPath = join(tmpDir, `${shot.id}.png`);
    if (shot.selector !== undefined) {
      await page.locator(shot.selector).screenshot({ path: pngPath });
    } else {
      await page.screenshot({ path: pngPath, fullPage: false });
    }

    const pngBuffer = readFileSync(pngPath);
    if (reportDir !== undefined) {
      mkdirSync(reportDir, { recursive: true });
      writeFileSync(join(reportDir, `${shot.id}.png`), pngBuffer);
    }

    const webp = await encodeWebp(pngBuffer);
    writeFileSync(join(IMG_DIR, `${shot.id}.webp`), webp);
    rmSync(pngPath, { force: true });

    return {
      id: shot.id,
      route,
      bytes: webp.byteLength,
      error: undefined,
    };
  } catch (error) {
    return {
      id: shot.id,
      route,
      bytes: undefined,
      error: error instanceof Error ? error.message : String(error),
    };
  } finally {
    await context?.close();
  }
}

function printTable(results: readonly CaptureResult[]): void {
  const idWidth = Math.max(2, ...results.map((r) => r.id.length));
  const bytesWidth = Math.max(
    5,
    ...results.map((r) => String(r.bytes ?? "MISSING").length),
  );
  const header = `${"id".padEnd(idWidth)} | ${"bytes".padEnd(bytesWidth)} | route`;
  console.log(header);
  console.log("-".repeat(header.length));
  for (const r of results) {
    const bytes = r.bytes !== undefined ? String(r.bytes) : "MISSING";
    console.log(
      `${r.id.padEnd(idWidth)} | ${bytes.padEnd(bytesWidth)} | ${r.route}`,
    );
    if (r.error !== undefined)
      console.log(`  ${" ".repeat(idWidth)} error: ${r.error}`);
  }
}

async function main(): Promise<void> {
  const cli = parseArgs(process.argv.slice(2));
  const shotsToRun =
    cli.only === undefined
      ? manifestShots
      : manifestShots.filter((shot) => (cli.only ?? []).includes(shot.id));

  if (shotsToRun.length === 0) {
    throw new Error(
      cli.only === undefined
        ? "shots.manifest.ts has no entries"
        : `--only matched nothing in shots.manifest.ts: ${(cli.only ?? []).join(", ")}`,
    );
  }

  const personasNeeded = new Set<ShotPersona>(
    shotsToRun.map((shot) => shot.persona ?? cli.persona),
  );
  ensureStorageStates(personasNeeded, cli.baseUrl);
  const seedMap = readSeedMap(cli.seedMapPath);
  if (cli.reportDir !== undefined) mkdirSync(cli.reportDir, { recursive: true });

  const tmpDir = mkdtempSync(join(tmpdir(), "docs-shots-"));
  const browser = await chromium.launch({
    headless: true,
    args: CHROMIUM_LAUNCH_OPTIONS.args,
  });
  const results: CaptureResult[] = [];
  try {
    for (const shot of shotsToRun) {
      console.log(`docs-shots: capturing ${shot.id} (${shot.route}) …`);
      const result = await captureShot(
        browser,
        shot,
        cli.baseUrl,
        cli.persona,
        tmpDir,
        seedMap,
        cli.reportDir,
      );
      results.push(result);
      if (result.error !== undefined)
        console.error(`docs-shots: ${shot.id} FAILED: ${result.error}`);
    }
  } finally {
    await browser.close();
    rmSync(tmpDir, { recursive: true, force: true });
  }

  console.log("");
  printTable(results);

  const missing = results.filter((r) => r.bytes === undefined);
  if (cli.failOnMissing && missing.length > 0) {
    console.error(
      `\ndocs-shots: --fail-on-missing set and ${missing.length} shot(s) produced no file: ` +
        missing.map((r) => r.id).join(", "),
    );
    process.exitCode = 1;
  }
}

main().catch((error: unknown) => {
  console.error(
    `docs-shots: ${error instanceof Error ? (error.stack ?? error.message) : String(error)}`,
  );
  process.exitCode = 1;
});
