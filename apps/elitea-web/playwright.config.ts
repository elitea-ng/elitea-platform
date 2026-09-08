/**
 * Playwright config (issue #60, unit V1).
 *
 * Projects:
 *   setup         — global setup: OIDC login per persona → saves storageState
 *   chromium      — every journey under e2e/journeys against the real
 *                   stack, chromium
 *   webkit        — the same journeys, webkit (spec §6.2)
 *
 * The count is deliberately NOT written here. It was "30" and had drifted:
 * journeys are discovered by the testMatch glob below, so a number in this
 * comment is a second source for one fact and the glob always wins.
 *   chat-stream   — the #284 chat journey, against the FULL standalone stack
 *                   (runtime plane + worker + mock LLM); see that project's
 *                   own note and `scripts/chat-stream-e2e.sh`
 *   index-stream  — the #93 index journey, same stack plus `seed-index`;
 *                   see `scripts/index-stream-e2e.sh`
 *   support-stack — the Support Assistant widget journey, same stack;
 *                   see `scripts/support-e2e.sh`
 *
 * The `setup` project runs once before the browser projects; each browser
 * project depends on it so the storageState files are always fresh.
 *
 * webServer: shells out to e2e-stack.sh up/down so `npx playwright test`
 * works standalone. With `reuseExistingServer: !CI` a developer who already
 * ran `./scripts/e2e-stack.sh up` skips the rebuild.
 */
import { defineConfig, devices } from '@playwright/test';
import path from 'path';
import { fileURLToPath } from 'url';

import {
  isLiveToolkitConfigured,
  liveImageModel,
  LIVE_TOOLKIT_IDS,
  type LiveToolkitId,
} from './e2e/live/liveEnv';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

/**
 * Where the per-persona signed-in states live. Overridable because cookies
 * minted against one stack reach every stack: the cookie domain is bare
 * `localhost`, which is PORT-AGNOSTIC, so two concurrent Playwright runs
 * against two local stacks overwrite each other's sessions through this
 * directory and fail each other's requests with 401 mid-run (measured).
 * A run against its own stack sets E2E_STATE_DIR to keep its states private.
 */
const STATE_DIR = process.env['E2E_STATE_DIR'] ?? path.join(__dirname, '.playwright-state');

export const STORAGE_STATE = {
  member: path.join(STATE_DIR, 'member.json'),
  admin: path.join(STATE_DIR, 'admin.json'),
  /**
   * The #284 chat driver. A persona of its own because it OWNS a personal
   * project, and the app selects the signed-in user's personal project over
   * the one `auth.setup.ts` writes to storage — handing member/admin one moves
   * every other journey off project 1 (measured, see the seeder's note).
   */
  chat: path.join(STATE_DIR, 'chat.json'),
};

/**
 * What `scripts/e2e-stack.sh seed` says it wrote into `centry.audit_events`:
 * the row count, the first and last timestamps, and the local day they fall on
 * (issue #214). Journey 29 freezes its browser clock to that day rather than
 * reading the wall clock, so the page's `Today` window and the fixture cannot
 * end up on opposite sides of a midnight.
 *
 * It lives beside the persona storageState because it is the same kind of
 * thing: per-run provisioning output, written by the seed, read by the tests,
 * gitignored, and inside the directory CI mounts into the Playwright container.
 */
export const AUDIT_FIXTURE_ANCHOR = path.join(STATE_DIR, 'audit-fixture.json');

// Default 8082 locally: centry legacy stack occupies 8080; CI sets E2E_PORT=8080.
export const BASE_URL = process.env['PLAYWRIGHT_BASE_URL'] ?? 'http://localhost:8082';

/**
 * The IANA zone EVERY browser context runs in, and the SAME zone
 * `scripts/e2e-stack.sh` computes its fixtures' start of day in (issue #214).
 *
 * Two clocks decide whether a seeded row falls inside a calendar-day filter,
 * and until now nothing made them agree:
 *
 *  - the BROWSER's. The admin Audit Trail opens on a `Today` preset
 *    (`src/pages/admin/auditFormat.ts`), whose `startOfDay` is
 *    `setHours(0,0,0,0)` on a `new Date()` — local midnight, in whatever zone
 *    the runner's OS happens to be in, sent to the server as an instant.
 *  - the DATABASE's. The audit fixture anchors its rows on
 *    `date_trunc('day', now())` so they land inside that window — in whatever
 *    zone the postgres session happens to be in.
 *
 * The two coincided in CI only because both sides inherited UTC by accident. At
 * 00:15 America/New_York they did not: three of the four fixture rows landed on
 * yesterday, and journey 29 failed "element not found" on rows that were
 * plainly in the table. Widening the filter or retrying would each have hidden
 * that. Pinning ONE zone and deriving BOTH day boundaries from it makes them
 * agree by construction, at every time of day.
 *
 * `E2E_TZ` overrides it, and must reach the seed and the browser TOGETHER —
 * `webServer.command` below runs the seed, so one exported variable does both.
 * Set it to a zone whose local time is a few minutes past midnight to run the
 * suite as if it were midnight, without touching any clock:
 *
 *   # at 13:05 UTC, Pacific/Guadalcanal (UTC+11) is 00:05 the next day
 *   E2E_TZ=Pacific/Guadalcanal npx playwright test --project=chromium
 *
 * UTC by default, so CI and every existing local invocation keep the day
 * boundary they already had. The difference is that they now HAVE one, rather
 * than borrowing whatever the runner and the database happened to be set to.
 */
export const E2E_TIMEZONE = process.env['E2E_TZ'] ?? 'UTC';

/*
 * Chromium-only launch flags — applied per project, never in the shared `use:`.
 *
 * They used to sit in the shared block, which handed them to WebKit as well.
 * Linux WebKit refuses to start on an unknown argument:
 *
 *   <launching> …/webkit-2336/pw_run.sh --disable-web-security …
 *   [err] Cannot parse arguments: Unknown option --disable-web-security
 *   Error: browserType.launch: Target page, context or browser has been closed
 *
 * — every webkit test failed at launch, on all three attempts, the first time
 * the suite ran in CI (issue #154). macOS WebKit tolerates the same flags, so
 * the whole webkit project passed locally while being unrunnable on Linux.
 * They are Chromium flags and have never meant anything to WebKit.
 */
/**
 * DeepWiki journeys that need the provider behind the facade. They run in
 * the ordinary chromium/webkit projects — the E2E stack composes the facade
 * under OIDC-only auth since ADR-0023 H0 — AND in the `deepwiki-stack`
 * project against the full standalone stack, which proves the same path
 * under production Form authentication.
 */
const PROVIDER_BACKED_JOURNEYS =
  /journeys\/deepwiki\/deepwiki\.(generation|chat|admission|wiki-query|folder-source)\.spec\.ts/;

/*
 * The admission journey (DWIKI-013) needs a stack that composes a PUBLIC
 * project (ELITEA_AI_PROJECT_ID): the facade registers its provider under it
 * at boot. The E2E stack composes none, by design, so the registrar logs a
 * skip there and the journey would have nothing to look at — it runs in the
 * `deepwiki-stack` project only, against the standalone stack.
 */
const ADMISSION_JOURNEY = /journeys\/deepwiki\/deepwiki\.admission\.spec\.ts/;

/*
 * The wiki_query journey (DWIKI-015) drives the provider through the FACADE
 * over the API — invoke, poll, read the composed objects. It needs the facade,
 * which only the standalone stack composes, and it DELETES a wiki it
 * generates, so it runs in the `deepwiki-stack` project only. On the E2E stack
 * every one of its invokes would answer 503 and the failure would read as a
 * broken family rather than as a stack that has no facade.
 */
const WIKI_QUERY_JOURNEY = /journeys\/deepwiki\/deepwiki\.wiki-query\.spec\.ts/;

/*
 * The real-engine journey (DWIKI-014) drives the copied analysis engine —
 * clone, index, plan, write — behind the Go host, on the standalone stack
 * with deploy/docker-compose.deepwiki-real-engine.yml applied
 * (scripts/deepwiki-real-engine.sh). Minutes long and image-heavy, so it has
 * its own project and no other project picks it up.
 */
const REAL_ENGINE_JOURNEY = /journeys\/deepwiki\/deepwiki\.real-engine\.spec\.ts/;

/*
 * The Support Assistant journey — the first E2E coverage of the in-app
 * widget itself (`admin/admin.features.spec.ts` only ever drove its ADMIN
 * section). A real turn is an agent execution, so it needs the FULL
 * standalone stack the same way `chat-stream` does; it runs in the
 * `support-stack` project only, against that stack
 * (`scripts/support-e2e.sh`).
 */
const SUPPORT_JOURNEY = /journeys\/support\/support\.spec\.ts/;

/*
 * The one journey that needs the shared project to hold NO toolkits: J17.1,
 * the empty-list redirect (`journeys/toolkits/toolkits.emptyList.spec.ts`).
 *
 * `fullyParallel` and a shared project 1 mean that emptiness is not a state
 * this journey can wait for — `toolkits.catalogue.spec.ts` alone holds one
 * toolkit per served category for the length of an eleven-minute test, and
 * the journey reported `expect(received).toBe(0)` received 8. It is ORDER
 * that makes the precondition true, so the file runs in its own project and
 * both engine projects depend on it: after `setup`, before the first journey
 * that can create a toolkit. See the file's own header.
 */
const EMPTY_TOOLKIT_LIST_JOURNEY = /journeys\/toolkits\/toolkits\.emptyList\.spec\.ts/;

/*
 * The fixture self-tests (API-FX1..4), and why they need their own project.
 *
 * API-FX3 calls `sweepAutotestEntities`, which deletes EVERY `autotest_`
 * conversation and agent in the shared project. That is the claim it is
 * about — a cleanup that reports success and deletes nothing is the failure
 * `admin.app-requests.spec.ts` (#544) and `toolkits.emptyList.spec.ts` were
 * both eventually rewritten for — and it is also, run at the wrong moment, a
 * way to destroy every sibling journey's rows mid-flight.
 *
 * So the sweep runs where it can only ever find its own: after `setup`, before
 * the first journey that can create an agent or a conversation, exactly the
 * slot `toolkits-empty` occupies for its own precondition. Both engine
 * projects depend on it and both ignore it, so it runs ONCE and never again.
 * The two projects can run together: this one touches conversations and
 * agents, that one asserts the toolkit count.
 *
 * A run of this project also clears whatever an earlier, killed run left in
 * the shared project — which is a second reason to want it first rather than
 * a reason to want it at all.
 */
const FIXTURE_ISOLATION_JOURNEY = /journeys\/api\/api\.fixture-isolation\.spec\.ts/;
/*
 * THE INVENTORY JOURNEYS (journeys/inventory, INV-001..010) HAVE NO CONSTANT
 * HERE, AND THAT IS THE DECISION.
 *
 * They are picked up by the `journeys/.+\.spec\.ts` glob into chromium and
 * webkit, and they run against the E2E STACK ONLY. That stack's
 * `elitea-inventory` service is the Go subapp host with
 * `ELITEA_INVENTORY_RUNNER=fixture`, whose canned graph
 * (services/elitea-subapp-host/internal/apps/inventory/run/fixture.go) holds
 * six entities and five relations — the data every one of those journeys
 * asserts on.
 *
 * There is deliberately NO `inventory-stack` project against the standalone
 * stack, the way DeepWiki has `deepwiki-stack`. The standalone stack reaches
 * Inventory through the PYTHON sidecar's fixture runner
 * (services/elitea-inventory/src/elitea_inventory/fixture_runner.py), which
 * answers an EMPTY graph — so every data assertion would fail there, correctly,
 * and a project that ran them would report a red suite for a stack doing
 * exactly what it is configured to do. Nothing is added to `testIgnore` for the
 * same reason in reverse: the journeys belong in the projects that CAN answer
 * them, and excluding them from anywhere would only hide that they ran.
 */

/*
 * ── THE LIVE LANES (e2e/live) ─────────────────────────────────────────────
 *
 * Two projects whose journeys need something no stack in this repository can
 * fake: a real toolkit credential, or a real image-capable model. They exist
 * so the legacy public suite's credential-bound cases are REUSED rather than
 * dropped — see `e2e/live/README.md` for the whole variable contract.
 *
 * THE GATE IS `testIgnore`, computed at config load, and that is the same
 * mechanic `chromium`/`webkit` use below to keep the stack-gated DeepWiki and
 * Support journeys out of themselves. A spec whose variables are absent is
 * ignored, so the project lists ZERO tests rather than listing a test that
 * would fail for want of a secret. There is deliberately no `test.skip`
 * anywhere under `e2e/live`: a skip is reported as a pass-shaped row, and a
 * lane that claims coverage it did not take is the one failure mode these
 * journeys were written to avoid.
 *
 * The observable contract, asserted by
 * `scripts/e2e-journey-shape.test.mjs`'s rule 5 and by the CI job:
 *
 *   with no E2E_LIVE_* set  ->  both projects list 0 tests
 *   with a provider set     ->  that provider's spec, and no other
 *
 * `--project=image-live` with no model therefore ends on Playwright's own
 * "no tests found" error and a non-zero exit, which is the loud failure the
 * lane owes rather than a silent green.
 *
 * Both need the FULL standalone stack with a real model — the
 * `chat-stream-real` shape — because every journey drives a real turn. They
 * run through `scripts/chat-stream-e2e.sh`, which forwards every `E2E_LIVE_*`
 * variable into the Playwright container.
 */
const LIVE_TOOLKIT_SPECS: Record<LiveToolkitId, RegExp> = {
  github: /live\/toolkits\.github\.spec\.ts/,
  jira: /live\/toolkits\.jira\.spec\.ts/,
  gitlab: /live\/toolkits\.gitlab\.spec\.ts/,
  bitbucket: /live\/toolkits\.bitbucket\.spec\.ts/,
  confluence: /live\/toolkits\.confluence\.spec\.ts/,
};

/** Both GitHub-gated: the two legacy agent files use a GitHub toolkit. */
const LIVE_AGENT_SPEC = /live\/toolkits\.agent\.spec\.ts/;
/** Jira-gated, as the legacy indicator file is. */
const LIVE_INDICATORS_SPEC = /live\/toolkits\.indicators\.spec\.ts/;
const LIVE_IMAGE_SPECS = /live\/image\..+\.spec\.ts/;

/** Every live toolkit spec whose provider this machine cannot drive. */
function unconfiguredLiveToolkitSpecs(): RegExp[] {
  const ignored: RegExp[] = [];
  for (const id of LIVE_TOOLKIT_IDS) {
    if (!isLiveToolkitConfigured(id)) ignored.push(LIVE_TOOLKIT_SPECS[id]);
  }
  if (!isLiveToolkitConfigured('github')) ignored.push(LIVE_AGENT_SPEC);
  if (!isLiveToolkitConfigured('jira')) ignored.push(LIVE_INDICATORS_SPEC);
  return ignored;
}

const CHROMIUM_LAUNCH_OPTIONS = {
  args: ['--disable-web-security', '--allow-insecure-localhost', '--no-sandbox'],
};

export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  forbidOnly: !!process.env['CI'],
  retries: process.env['CI'] ? 2 : 0,
  workers: process.env['CI'] ? 4 : undefined,
  reporter: process.env['CI']
    ? [['github'], ['html', { open: 'never' }]]
    : [['list'], ['html', { open: 'never' }]],

  use: {
    baseURL: BASE_URL,
    // Shared, not per-project: every project drives the same seeded stack, so
    // every project must read the same day boundary out of it. See E2E_TIMEZONE.
    timezoneId: E2E_TIMEZONE,
    // Trace on retry so failures in CI have full context.
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },

  projects: [
    // ── Auth setup — runs before any browser project ──────────────────────
    /*
     * `fullyParallel: false`: THE THREE SIGN-INS COMPETE FOR ONE SERVER-SIDE
     * RESOURCE, so they may not run at the same time.
     *
     * Every sign-in starts the caller's personal project
     * (`ensurePersonalProject`, services/elitea-main/internal/api/v2/auth/
     * signin.go). The provisioner runs ONE attempt at a time
     * (`maxConcurrentProvisions = 1`, internal/application/personalproject/
     * ensurer.go) and, when that single slot is taken, it DROPS the attempt
     * instead of queueing it — safe for a browser, which polls
     * `/social/author` until the id arrives, and NOT safe here, because for
     * these two personas that poll never asks again: `resolvePersonalProjectID`
     * falls back to "the lowest-id project the user holds a role in", which is
     * the seeded project 1, and a non-empty answer stops `GET /social/author`
     * from re-arming the provisioner. The persona that lost the race therefore
     * has no personal project for the WHOLE run.
     *
     * Under the top-level `fullyParallel: true` the three tests below went to
     * three workers, so which persona won was decided by worker scheduling and
     * held for the rest of the run. That is the two stable states issue #839
     * describes: the rail on every list page shows "Trending Authors" when the
     * member persona owns a personal project and its own author card when
     * `personal_project_id` collapses onto the selected project 1, and the
     * admin budgets table gains or loses that persona's "Personal project"
     * row. Both states are self-consistent, so a re-recorded baseline is
     * simply the state of the run that recorded it.
     *
     * Sequential, plus the per-persona wait `auth.setup.ts` now makes, means
     * each sign-in finds the slot free — so EVERY persona ends the setup owning
     * a personal project, which is what the product promises a signed-in user.
     */
    {
      name: 'setup',
      testMatch: /auth\.setup\.ts/,
      fullyParallel: false,
      use: { ...devices['Desktop Chrome'], launchOptions: CHROMIUM_LAUNCH_OPTIONS },
    },

    /*
     * ── toolkits-empty — the empty-list redirect, before anything seeds ────
     *
     * Ordering IS this journey's fixture: it asserts that the shared project
     * holds no toolkits and that the app redirects because of it, and every
     * other toolkit/MCP journey creates rows in that project. Running it as a
     * dependency of both engine projects is what gives it the state it is
     * about — it runs after `setup` and before the first sibling starts, and
     * it creates nothing, so the stack it hands on is the one it found.
     *
     * Chromium only: a redirect driven by a JSON count is not a rasteriser
     * question, and a second engine would only widen the serial section every
     * other journey waits behind.
     */
    {
      name: 'toolkits-empty',
      use: {
        ...devices['Desktop Chrome'],
        storageState: STORAGE_STATE.member,
        launchOptions: CHROMIUM_LAUNCH_OPTIONS,
      },
      dependencies: ['setup'],
      testMatch: EMPTY_TOOLKIT_LIST_JOURNEY,
    },

    /*
     * ── fixtures-sweep — the fixture self-tests, before anything seeds ─────
     *
     * See `FIXTURE_ISOLATION_JOURNEY` above. Serial, because API-FX3's sweep
     * would otherwise take the rows API-FX1/2/4 are holding; chromium only,
     * because none of the four opens a page.
     */
    {
      name: 'fixtures-sweep',
      use: {
        ...devices['Desktop Chrome'],
        storageState: STORAGE_STATE.admin,
        launchOptions: CHROMIUM_LAUNCH_OPTIONS,
      },
      dependencies: ['setup'],
      testMatch: FIXTURE_ISOLATION_JOURNEY,
      fullyParallel: false,
    },

    // ── chromium ──────────────────────────────────────────────────────────
    {
      name: 'chromium',
      testIgnore: [
        ADMISSION_JOURNEY,
        REAL_ENGINE_JOURNEY,
        WIKI_QUERY_JOURNEY,
        SUPPORT_JOURNEY,
        EMPTY_TOOLKIT_LIST_JOURNEY,
        FIXTURE_ISOLATION_JOURNEY,
      ],
      use: {
        ...devices['Desktop Chrome'],
        storageState: STORAGE_STATE.member,
        launchOptions: CHROMIUM_LAUNCH_OPTIONS,
      },
      dependencies: ['setup', 'toolkits-empty', 'fixtures-sweep'],
      testMatch: /journeys\/.+\.spec\.ts/,
    },

    // ── webkit (spec §6.2: "chromium + webkit") ───────────────────────────
    {
      name: 'webkit',
      testIgnore: [
        ADMISSION_JOURNEY,
        REAL_ENGINE_JOURNEY,
        WIKI_QUERY_JOURNEY,
        SUPPORT_JOURNEY,
        EMPTY_TOOLKIT_LIST_JOURNEY,
        FIXTURE_ISOLATION_JOURNEY,
      ],
      use: {
        ...devices['Desktop Safari'],
        storageState: STORAGE_STATE.member,
      },
      dependencies: ['setup', 'toolkits-empty', 'fixtures-sweep'],
      testMatch: /journeys\/.+\.spec\.ts/,
    },

    /*
     * ── visual (release tags only, via `--grep @visual`) ───────────────────
     *
     * Its own project because the journey projects match `journeys/**` only —
     * `e2e/visual/**` was invisible to them, and `--grep @visual` reported
     * "No tests found" while looking like a suite that simply had nothing to
     * report.
     *
     * Chromium ONLY, deliberately: a baseline is valid for exactly one
     * rasteriser, and running webkit against chromium's PNGs would diff every
     * screen for reasons unrelated to the UI. Adding webkit means a second set
     * of baselines, not a second project against the same set.
     */
    {
      name: 'visual',
      use: {
        ...devices['Desktop Chrome'],
        storageState: STORAGE_STATE.member,
        launchOptions: CHROMIUM_LAUNCH_OPTIONS,
        // Pinned so a snapshot's geometry never depends on the runner's
        // defaults. Matches the 1602x848 CSS geometry of the production
        // reference set in parity/screenshot-index.json.
        viewport: { width: 1602, height: 848 },
        deviceScaleFactor: 2,
      },
      dependencies: ['setup'],
      testMatch: /visual\/.+\.spec\.ts/,
    },

    /*
     * ── chat-stream (#284) — the chat definition-of-done journey ───────────
     *
     * Its own project because it is the one journey that cannot run against
     * `docker-compose.e2e-standalone.yml`: an agent turn needs the runtime
     * plane, the worker and a model backend, none of which that stack has.
     * `scripts/chat-stream-e2e.sh` brings up the FULL standalone stack and
     * points this project at it, so it never runs by accident against a stack
     * that would fail it for the wrong reason.
     *
     * Chromium only: this asserts a transport and a render, not a rasteriser,
     * and a second engine would double the stack time for no new signal.
     */
    {
      /**
       * The DeepWiki journeys that need the PROVIDER: generation, chat,
       * delete-after-generation. They run against the FULL standalone stack
       * (scripts/deepwiki-e2e.sh), the only stack where the facade can be
       * composed — it needs production Form authentication, which the E2E
       * stack's OIDC-only mode does not have. Serial: they share toolkit 9002.
       */
      name: 'deepwiki-stack',
      use: {
        ...devices['Desktop Chrome'],
        storageState: STORAGE_STATE.member,
        launchOptions: CHROMIUM_LAUNCH_OPTIONS,
      },
      dependencies: ['setup'],
      testMatch: PROVIDER_BACKED_JOURNEYS,
      fullyParallel: false,
    },
    {
      /**
       * The Support Assistant journey (`e2e/journeys/support/support.spec.ts`)
       * — the first E2E coverage that opens the in-app widget rather than
       * only its admin section. A real turn needs an agent to answer, which
       * needs the FULL standalone stack the same way `chat-stream` does.
       *
       * `storageState: STORAGE_STATE.admin`: the seeding needs an admin
       * session (create the agent, PUT the admin Features section for
       * `support_assistant`), and the same session then opens the widget as
       * an ordinary authenticated user — the support project enrols any
       * caller as a viewer on first use, admin included, so one persona
       * covers both jobs.
       *
       * `fullyParallel: false`: the file's three tests share state across
       * the run (a conversation started in test 2 is read back in test 3)
       * and a platform-wide admin section (test 1 must run BEFORE test 2
       * turns the switch on). The top-level `fullyParallel: true` would
       * otherwise let Playwright run them out of order or in different
       * workers.
       */
      name: 'support-stack',
      use: {
        ...devices['Desktop Chrome'],
        storageState: STORAGE_STATE.admin,
        launchOptions: CHROMIUM_LAUNCH_OPTIONS,
      },
      dependencies: ['setup'],
      testMatch: SUPPORT_JOURNEY,
      fullyParallel: false,
    },
    {
      /**
       * DWIKI-014: the REAL engine, end to end (scripts/deepwiki-real-engine.sh).
       * Serial and alone: one generation is minutes of clone/index/write.
       */
      name: 'deepwiki-real-engine',
      use: {
        ...devices['Desktop Chrome'],
        storageState: STORAGE_STATE.member,
        launchOptions: CHROMIUM_LAUNCH_OPTIONS,
      },
      dependencies: ['setup'],
      testMatch: REAL_ENGINE_JOURNEY,
      fullyParallel: false,
      retries: 0,
    },
    {
      name: 'chat-stream',
      use: {
        ...devices['Desktop Chrome'],
        storageState: STORAGE_STATE.chat,
        launchOptions: CHROMIUM_LAUNCH_OPTIONS,
      },
      dependencies: ['setup'],
      // Pinned to `chat.*` rather than all of `streaming/**`: this directory now
      // holds two journeys with DIFFERENT seeding needs, and a broad match here
      // would hand the index journey to a runner that never ran `seed-index`.
      testMatch: /streaming\/chat\..+\.spec\.ts/,
      /*
       * Serial, for the same class of reason as `index-stream` below and one
       * specific number: elitea-main admits FOUR concurrent replay streams per
       * principal (`internal/api/v2/executions/events_admission.go`), and every
       * journey in this project signs in as the same chat persona. Each one
       * holds an execution stream for the length of a turn while the app also
       * holds its notifications stream, so two of them in parallel sit on the
       * edge of that budget. Measured: run together over three workers, the
       * second journey's `/executions/{id}/events` answered 429 and the failure
       * read as "the browser cannot read the stream" — a statement about the
       * harness, not the feature. Serially, they pass.
       *
       * WHERE THE SERIALISATION ACTUALLY COMES FROM: `--workers=1`, passed on
       * the `chat-stream` invocation in `scripts/chat-stream-e2e.sh`. That
       * script is the ONLY entry point — both continuous-integration jobs
       * (`chat-stream` and `chat-stream-rust` in .github/workflows/ci-web-e2e.yml)
       * run it, and so does every local run — so the pin lives there once and
       * covers all of them.
       *
       * `fullyParallel: false` does NOT do it, and it is important not to read
       * this line as if it did. It orders tests WITHIN a file. Every spec this
       * project matches holds exactly ONE test in a file of its own, so there
       * has never been anything for it to order, and `workers: 4` above still
       * started all three files at once — against the very budget this note is
       * about. It is kept because it costs nothing and becomes correct the
       * moment one of those files grows a second test.
       */
      fullyParallel: false,
    },

    /*
     * ── chat-stream-real — the structural chat journeys against a REAL model ─
     *
     * The same stack as `chat-stream` with the `real-llm` compose profile
     * (deploy/docker-compose.standalone-full.yml, service `llm-real`): a
     * digest-pinned llama.cpp image serving Qwen3-0.6B, seeded through the
     * self-hosted path and named to the specs by E2E_CHAT_MODEL. Run by the
     * `chat-stream-real` job in .github/workflows/ci-web-e2e.yml through
     * scripts/chat-stream-e2e.sh with PLAYWRIGHT_PROJECT=chat-stream-real.
     *
     * An EXPLICIT list, not a regex, because membership is a per-spec
     * judgement that a filename cannot express. A spec belongs here only when
     * every assertion it makes holds against text nobody scripted: no echo,
     * no `[[mock:…]]` marker, no sentinel, no journal read, no timing keyed
     * on MOCK_LLM_CHUNK_DELAY_MS. The audit that placed each one lives in
     * the `llm-real` service comment; the short version:
     *
     *   admin-providers   no model turn at all; the live check branches on
     *                     the captured verdict, so any upstream passes
     *   agent             answer asserted non-empty, stored row without
     *                     `contains`
     *   mcp               stored non-error answer; the refusal string it
     *                     matches is the runtime's, not the model's
     *   pipeline*         trace must name the LLM node(s); every one of them
     *                     already says "the graph runs a real model, which
     *                     echoes nothing" — they NEED E2E_WORKER=rust, which
     *                     is why the job runs on STANDALONE_WORKER=rust
     *   regenerate        the echo `contains` is relaxed under
     *                     E2E_CHAT_MODEL; the regeneration half polls
     *                     `execution_generation`, never text
     *
     * Deliberately NOT here, and not because they are mock-bound:
     *   nested-agent      its prompt ORDERS delegation; the mock never
     *                     takes the offer, a real model will, and the spec
     *                     fails on the child's `is_error` — real signal,
     *                     but a flake by construction at 0.6B
     *   agent-tools       arms seven internal tools for a 0.6B model to
     *                     mis-call; same shape of flake
     * Both are the first candidates once a larger model or a judgement on
     * tool-call quality exists. The nine echo/marker/journal specs (stop,
     * hitl, toolkit, toolkit-hitl, variables, attachments, streaming,
     * multiturn) stay on `chat-stream`; the mock is what makes them
     * assertable at all.
     *
     * Serial for the same execution-stream budget as `chat-stream`, and
     * through the same `--workers=1` pin in the script.
     */
    {
      name: 'chat-stream-real',
      use: {
        ...devices['Desktop Chrome'],
        storageState: STORAGE_STATE.chat,
        launchOptions: CHROMIUM_LAUNCH_OPTIONS,
      },
      dependencies: ['setup'],
      testMatch: [
        /streaming\/chat\.admin-providers\.spec\.ts/,
        /streaming\/chat\.agent\.spec\.ts/,
        /streaming\/chat\.mcp\.spec\.ts/,
        /streaming\/chat\.pipeline\.spec\.ts/,
        /streaming\/chat\.pipeline-authored\.spec\.ts/,
        /streaming\/chat\.pipeline-multinode\.spec\.ts/,
        /streaming\/chat\.regenerate\.spec\.ts/,
      ],
      fullyParallel: false,
    },

    /*
     * ── chat-stream-real-nightly — the real-model lane's exploratory superset ─
     *
     * Everything in `chat-stream-real` plus the two specs it deliberately
     * leaves out, `nested-agent` and `agent-tools`: the ones whose prompts
     * OFFER a tool call that the mock never takes and a real model will. On a
     * pull request that is a flake by construction at 0.6B; on a schedule it
     * is the measurement that decides when they can join the PR lane. Run by
     * .github/workflows/nightly-real-llm.yml on both runtimes.
     */
    {
      name: 'chat-stream-real-nightly',
      use: {
        ...devices['Desktop Chrome'],
        storageState: STORAGE_STATE.chat,
        launchOptions: CHROMIUM_LAUNCH_OPTIONS,
      },
      dependencies: ['setup'],
      testMatch: [
        /streaming\/chat\.admin-providers\.spec\.ts/,
        /streaming\/chat\.agent\.spec\.ts/,
        /streaming\/chat\.agent-tools\.spec\.ts/,
        /streaming\/chat\.mcp\.spec\.ts/,
        /streaming\/chat\.nested-agent\.spec\.ts/,
        /streaming\/chat\.pipeline\.spec\.ts/,
        /streaming\/chat\.pipeline-authored\.spec\.ts/,
        /streaming\/chat\.pipeline-multinode\.spec\.ts/,
        /streaming\/chat\.regenerate\.spec\.ts/,
      ],
      fullyParallel: false,
    },

    /*
     * ── toolkits-live — the legacy credential-bound toolkit cases ──────────
     *
     * See the LIVE LANES note above and `e2e/live/README.md`. Serial and on
     * the chat persona for the same two reasons `chat-stream` states: the
     * turns run through the same single-consumer execution plane, and the
     * personal project is where `/llm` resolves the provider credential from
     * (#290).
     */
    {
      name: 'toolkits-live',
      use: {
        ...devices['Desktop Chrome'],
        storageState: STORAGE_STATE.chat,
        launchOptions: CHROMIUM_LAUNCH_OPTIONS,
      },
      dependencies: ['setup'],
      testMatch: /live\/toolkits\..+\.spec\.ts/,
      testIgnore: unconfiguredLiveToolkitSpecs(),
      fullyParallel: false,
      retries: 0,
    },

    /*
     * ── image-live — image creation against a real image-capable model ─────
     *
     * `deploy/mock-llm/server.py` serves chat completions and nothing else,
     * so these two cannot run on any other project. `E2E_LIVE_IMAGE_MODEL`
     * names the model row as the picker spells it; without it the project
     * lists nothing and a `--project=image-live` run fails on Playwright's
     * own "no tests found".
     */
    {
      name: 'image-live',
      use: {
        ...devices['Desktop Chrome'],
        storageState: STORAGE_STATE.chat,
        launchOptions: CHROMIUM_LAUNCH_OPTIONS,
      },
      dependencies: ['setup'],
      testMatch: LIVE_IMAGE_SPECS,
      testIgnore: liveImageModel() === '' ? [LIVE_IMAGE_SPECS] : [],
      fullyParallel: false,
      retries: 0,
    },

    /*
     * ── index-stream (#93 Surface A) — the index definition-of-done journey ──
     *
     * Its own project for the same reason `chat-stream` is: it needs the FULL
     * standalone stack (runtime plane + agent worker), plus the `seed-index`
     * rows. `scripts/index-stream-e2e.sh` provisions both and points this
     * project at the result, so it never runs by accident against a stack that
     * would fail it for the wrong reason.
     *
     * Shares `STORAGE_STATE.chat`: that persona OWNS a personal project, and
     * the index permissions — including `models.applications.index_meta.details`,
     * which project 1 does NOT grant — are seeded there.
     *
     * Chromium only: this asserts a transport and a render, not a rasteriser.
     */
    {
      name: 'index-stream',
      use: {
        ...devices['Desktop Chrome'],
        storageState: STORAGE_STATE.chat,
        launchOptions: CHROMIUM_LAUNCH_OPTIONS,
      },
      dependencies: ['setup'],
      testMatch: /streaming\/index\..+\.spec\.ts/,
      /*
       * Serial, overriding the top-level `fullyParallel`. These two tests drive
       * the SAME toolkit through the SAME single-consumer execution plane, so
       * running them against each other measures the stack's concurrency, not
       * the feature. Measured on the standalone stack: with the suite spread
       * over three workers, elitea-main's pool saturated and
       * `list project configurations` timed out, which surfaced as the start
       * POST never being answered — a 40s `waitForResponse` timeout that reads
       * exactly like "the Index button does nothing". Serially, 3x3 green.
       */
      fullyParallel: false,
    },
  ],

  // Bring up the real E2E stack before the test run.
  webServer: {
    // `up` exits once all services are healthy; `seed` then applies the DB
    // schema and creates the OIDC personas + RBAC grants that auth.setup logs
    // in as. Running only `up` left every persona non-existent, so no clean run
    // could authenticate (PR #82 validation, blocker 5).
    command: `${__dirname}/scripts/e2e-stack.sh up && ${__dirname}/scripts/e2e-stack.sh seed`,
    url: BASE_URL + '/app/',
    /*
     * `E2E_REUSE_STACK=1` forces reuse regardless of CI (issue #61, item 3).
     *
     * The visual job cannot use the plain `!CI` rule. Baselines are only valid
     * when rendered inside the pinned `mcr.microsoft.com/playwright:v1.62.0-noble`
     * image — a bare `npx playwright test` on the runner's own OS rasterises
     * fonts differently and every snapshot diffs. But the stack itself needs a
     * container runtime, so it has to come up on the HOST and the test run goes
     * into the container with `--network host`. In that shape `CI` is set, so
     * `reuseExistingServer` would be false and Playwright would try to bring the
     * stack up a second time — from inside a container that has no podman/docker.
     *
     * A job container instead of `docker run --network host` does not work
     * either: GitHub job containers do not share the runner's network namespace,
     * so `BASE_URL=http://localhost:8082` would not reach a host-side stack.
     */
    reuseExistingServer: process.env['E2E_REUSE_STACK'] === '1' || !process.env['CI'],
    // Allow up to 3 minutes for image builds + postgres migrations on a cold start.
    timeout: 180_000,
  },

  outputDir: 'playwright-results',
  snapshotDir: 'e2e/snapshots',
});
