/**
 * The platform-flag lock (issue #519).
 *
 * ## The problem this exists for
 *
 * `mcp_enabled` is ONE row for the whole deployment, and journey 36b exists to
 * prove that turning it off changes the platform. While it is off, the platform
 * IS changed — for every other journey as well. The MCP surfaces disappear:
 * `useIsMcpVisible()` returns false, `ToolkitTypeSelector` returns null, the
 * `/mcps` route is closed, and `ToolBase` stops drawing the "Make tools
 * available by MCP" field.
 *
 * `fullyParallel` is on, so a different set of journeys was inside that window
 * on every run. Measured in this repository, on one tree:
 *
 *  - journey 17.3 failing on the missing MCP checkbox, 2 runs in 10;
 *  - the two MCP journeys of JRNY-018 failing on an empty catalogue and an
 *    absent search box, in three separate CI runs of `E2E (webkit)`.
 *
 * A mutex over the WRITERS alone cannot fix that: the readers never took it.
 *
 * ## The lock
 *
 * One writer, many readers, over the filesystem, because the readers are in
 * different worker PROCESSES (and, when the suite is run locally with both
 * engines, in different browser projects against one stack).
 *
 *  - the writer is a directory. `mkdir` fails atomically on an existing
 *    directory on every platform, which is the whole mechanism, and it needs no
 *    dependency — the reason lockfiles have used it for decades.
 *  - each reader is a file in `readers/`. The writer waits for that directory
 *    to drain before it enters.
 *
 * A reader creates its file and then LOOKS AGAIN for the writer. That second
 * look is what closes the window between "no writer" and "my file exists": a
 * writer that arrived in between is seen, the reader removes its file and backs
 * off, and the writer — which was already waiting on that same file — goes
 * through. Neither side can pass the other.
 *
 * Nothing here can wedge the suite. Both sides break a token that has stopped
 * BEATING, and both sides give up waiting at a far outer bound and continue,
 * because a lock that can stop CI is a worse failure than the race it prevents.
 *
 * ## The heartbeat, and why waiting on a wall clock was wrong
 *
 * The writer token used to carry one timestamp: the moment it was created. A
 * waiter gave up after `STALE_MS` and DELETED it, and a live holder that was
 * still working — or a queue three tests deep — was indistinguishable from a
 * worker that had died. Measured: the four `support.widget.spec.ts` tests each
 * hold the window for about half a minute, so the third or fourth to be
 * scheduled always reached the 90 s bound, broke a live holder, entered, and
 * then turned the assistant OFF in its own `finally` while its sibling still
 * had the page open. The sibling's launcher vanished and the failure read as
 * "the widget never rendered" (`support.widget.spec.ts` SUP-W2/SUP-W4, both
 * engines, one victim per run and a different one each time).
 *
 * The holder now touches `writer/heartbeat` every `BEAT_MS`, so "alive" is a
 * fact rather than an estimate: a token whose beat is older than
 * `HEARTBEAT_STALE_MS` belongs to a process that is gone, and one that is
 * beating is left alone however long the work takes.
 *
 * ## The group, and why a file may share one window
 *
 * Tests that write the SAME flags to the SAME values do not need to exclude
 * each other — they need to exclude everyone else. `withPlatformFlagLock`
 * takes an optional `group`: the first member creates the window, later
 * members of the same group JOIN it, and the last one out closes it (running
 * `onLastExit` first, while the window is still held, so the flags are back to
 * their default before any other journey can be inside). That turns the
 * support file's four windows into one, which is three fewer windows for every
 * other flag-writing journey to queue behind.
 */
import { randomUUID } from 'node:crypto';
import { mkdir, readdir, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const ROOT = join(tmpdir(), 'elitea-e2e-platform-flags');
const WRITER = join(ROOT, 'writer');
const READERS = join(ROOT, 'readers');
/** Touched by every member of the window while it works — see the header. */
const HEARTBEAT = join(WRITER, 'heartbeat');
/** Names the window's group, so a sibling can tell whether it may join. */
const GROUP = join(WRITER, 'group');
/** One file per member of the window. The window closes when it is empty. */
const MEMBERS = join(WRITER, 'members');

/** A reader token older than this belongs to a run that is gone. */
const STALE_MS = 90_000;
/** How often a holder proves it is still there. */
const BEAT_MS = 2_000;
/**
 * A window whose beat is older than this is orphaned.
 *
 * Several beats of slack, so a worker that is merely busy — a long
 * `page.goto`, a saturated CI runner — is never mistaken for a dead one.
 */
const HEARTBEAT_STALE_MS = 20_000;
/**
 * The outer bound on WAITING for a window, after which the waiter breaks it.
 *
 * It is far past any honest window (a journey that holds one has its own
 * 210 s test timeout) and exists only so a shape nobody has thought of yet
 * cannot stop CI. The heartbeat is what actually frees a waiter.
 */
const WAIT_LIMIT_MS = 240_000;
const POLL_MS = 100;

const sleep = async (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms));

async function ensureDirs(): Promise<void> {
  await mkdir(ROOT, { recursive: true });
  await mkdir(READERS, { recursive: true });
}

/** How long ago the window last proved it was alive, or `undefined` if there is none. */
async function beatAge(): Promise<number | undefined> {
  try {
    // The heartbeat file, and the directory itself for the instant between
    // `mkdir` and the first beat.
    const info = await stat(HEARTBEAT).catch(async () => stat(WRITER));
    return Date.now() - info.mtimeMs;
  } catch {
    return undefined;
  }
}

/** True while a live writer holds the lock. Breaks an ORPHANED one on the way. */
async function writerHeld(): Promise<boolean> {
  const age = await beatAge();
  if (age === undefined) return false;
  if (age > HEARTBEAT_STALE_MS) {
    await rm(WRITER, { recursive: true, force: true });
    return false;
  }
  return true;
}

/** The group name this window belongs to, or `undefined` when it is exclusive. */
async function windowGroup(): Promise<string | undefined> {
  try {
    return (await readFile(GROUP, 'utf8')).trim();
  } catch {
    return undefined;
  }
}

/** How many members are still inside the window. */
async function memberCount(): Promise<number> {
  try {
    return (await readdir(MEMBERS)).length;
  } catch {
    return 0;
  }
}

/** Starts this member's beat. The timer is unref'd so it can never hold a worker open. */
function startBeating(): NodeJS.Timeout {
  const timer = setInterval(() => {
    void writeFile(HEARTBEAT, String(Date.now())).catch(() => {
      // The window was closed under us. `release` is idempotent and the next
      // beat is harmless either way.
    });
  }, BEAT_MS);
  timer.unref();
  return timer;
}

/** The reader tokens that are still live. Removes the ones that are not. */
async function liveReaders(): Promise<number> {
  let names: string[] = [];
  try {
    names = await readdir(READERS);
  } catch {
    return 0;
  }
  let live = 0;
  for (const name of names) {
    const path = join(READERS, name);
    try {
      const info = await stat(path);
      if (Date.now() - info.mtimeMs > STALE_MS) {
        await rm(path, { force: true });
        continue;
      }
      live += 1;
    } catch {
      // Removed while it was read. It is not live.
    }
  }
  return live;
}

/** How a caller shares one window with its siblings — see the file header. */
export interface PlatformFlagLockOptions {
  /**
   * Tests carrying the same group name share ONE window instead of queueing.
   * Use it only where every member writes the same flags to the same values:
   * the group's members are concurrent with each other and exclusive of
   * everyone else.
   */
  readonly group?: string;
  /**
   * Run by the LAST member to leave, while the window is still held, so the
   * flags this group turned on are off again before any other journey can be
   * inside. It is not run at all when a sibling is still working.
   */
  readonly onLastExit?: () => Promise<void>;
}

/** This member's own token file, plus the beat that keeps the window alive. */
interface WindowMembership {
  readonly token: string;
  readonly beat: NodeJS.Timeout;
}

/** Creates the window. `undefined` when another one already exists. */
async function openWindow(group: string | undefined): Promise<WindowMembership | undefined> {
  try {
    await mkdir(WRITER);
  } catch {
    return undefined;
  }
  await writeFile(HEARTBEAT, String(Date.now()));
  if (group !== undefined) await writeFile(GROUP, group);
  await mkdir(MEMBERS, { recursive: true });
  const token = join(MEMBERS, `${String(process.pid)}-${randomUUID()}`);
  await writeFile(token, String(Date.now()));
  return { token, beat: startBeating() };
}

/**
 * Joins an OPEN window of the same group. `undefined` when there is none.
 *
 * The token is written and the group then read AGAIN — the same double look
 * the readers take. A window that closed between the two is one this member
 * must not be inside, so the token is withdrawn and the caller goes back to
 * waiting.
 */
async function joinWindow(group: string): Promise<WindowMembership | undefined> {
  if (!(await writerHeld()) || (await windowGroup()) !== group) return undefined;
  const token = join(MEMBERS, `${String(process.pid)}-${randomUUID()}`);
  try {
    await writeFile(token, String(Date.now()));
  } catch {
    return undefined;
  }
  if ((await windowGroup()) === group && (await writerHeld())) {
    return { token, beat: startBeating() };
  }
  await rm(token, { force: true });
  return undefined;
}

/**
 * Runs `body` with the platform flags held: no reader that took
 * `readsPlatformFlags` is inside the window, and no other writer is either —
 * except a sibling of the same `group`, which shares this one.
 */
export async function withPlatformFlagLock<T>(
  body: () => Promise<T>,
  options: PlatformFlagLockOptions = {},
): Promise<T> {
  await ensureDirs();

  const { group, onLastExit } = options;
  const deadline = Date.now() + WAIT_LIMIT_MS;
  let membership: WindowMembership | undefined;
  let opened = false;

  for (;;) {
    membership = await openWindow(group);
    if (membership !== undefined) {
      opened = true;
      break;
    }
    if (group !== undefined) {
      membership = await joinWindow(group);
      if (membership !== undefined) break;
    }
    // Also breaks an ORPHANED window, so this cannot spin for ever.
    await writerHeld();
    if (Date.now() > deadline) {
      await rm(WRITER, { recursive: true, force: true });
      continue;
    }
    await sleep(POLL_MS);
  }

  try {
    // Only the member that OPENED the window waits for the readers: a sibling
    // joining one that is already open would be waiting for a drain that
    // happened before it arrived.
    if (opened) {
      const drainBy = Date.now() + STALE_MS;
      while ((await liveReaders()) > 0 && Date.now() < drainBy) {
        await sleep(POLL_MS);
      }
    }
    return await body();
  } finally {
    clearInterval(membership.beat);
    // The group marker goes FIRST, so a sibling that has not yet looked cannot
    // join a window that is about to close.
    if ((await memberCount()) <= 1) await rm(GROUP, { force: true });
    await rm(membership.token, { force: true });
    if ((await memberCount()) === 0) {
      if (onLastExit) await onLastExit();
      await rm(WRITER, { recursive: true, force: true });
    }
  }
}

/**
 * Takes the SHARED lock and returns the release.
 *
 * Take it in a journey that READS a platform flag — the MCP surfaces are the
 * ones that exist today. Readers never wait for each other, so outside journey
 * 36's window the cost is one `stat` and one file.
 */
export async function acquirePlatformFlagRead(): Promise<() => Promise<void>> {
  await ensureDirs();

  const token = join(READERS, `${String(process.pid)}-${randomUUID()}`);
  const deadline = Date.now() + STALE_MS;

  for (;;) {
    while ((await writerHeld()) && Date.now() < deadline) {
      await sleep(POLL_MS);
    }
    await writeFile(token, String(Date.now()));
    if (!(await writerHeld())) break;
    // A writer arrived between the two looks. Stand aside; it is already
    // waiting for this token.
    await rm(token, { force: true });
    if (Date.now() > deadline) break;
    await sleep(POLL_MS);
  }

  return async () => {
    await rm(token, { force: true });
  };
}

/**
 * What a `beforeEach` needs from `TestInfo` to lengthen its own budget.
 *
 * Structural rather than Playwright's `TestInfo` so this module stays a plain
 * Node one — it is imported by the lock's own vitest suite, which has no
 * Playwright runtime.
 */
interface TimeoutExtendable {
  readonly timeout: number;
  setTimeout: (timeout: number) => void;
}

/**
 * Registers the shared lock around every test of the calling file.
 *
 * A hook pair rather than a wrapper so a spec file states the dependency once,
 * at the top instead of indenting every body — and so a test added to that
 * file later cannot forget it. Module state is safe: a worker process runs one
 * test at a time.
 *
 * THE HOOK LENGTHENS THE TEST'S BUDGET BY ITS OWN WAIT BOUND. Waiting is the
 * normal case, not the exceptional one: journey 36b and the guardrails journey
 * hold windows measured in tens of seconds, and the support file holds one
 * window for four tests. A reader may therefore legitimately wait up to
 * `STALE_MS`, while the hook runs INSIDE the test's timeout — 30 s for most
 * projects. Without this the waiter is killed by the test clock and reports
 * "Test timeout of 30000ms exceeded while running "beforeEach" hook", which
 * names this file and says nothing about the writer it was waiting for.
 * Measured that way on `toolkits.catalogue.spec.ts` (webkit).
 *
 * Lengthening rather than shortening the wait is deliberate. A reader that
 * gave up early would go on to run INSIDE the writer's window, which is
 * exactly the failure this lock exists to prevent (#519) — and it would fail
 * as "the MCP checkbox is missing", blaming the product.
 *
 * A timeout of `0` means the project disabled the clock; adding to it would
 * impose one.
 */
export function readsPlatformFlags(test: {
  beforeEach: (fn: (args: Record<string, never>, testInfo: TimeoutExtendable) => Promise<void>) => void;
  afterEach: (fn: () => Promise<void>) => void;
}): void {
  let release: (() => Promise<void>) | undefined;

  /* Playwright reads the first parameter's SOURCE and requires an object
     pattern there; a named one is refused before the file is even listed. */
  // eslint-disable-next-line no-empty-pattern
  test.beforeEach(async ({}: Record<string, never>, testInfo: TimeoutExtendable) => {
    if (testInfo.timeout > 0) testInfo.setTimeout(testInfo.timeout + STALE_MS);
    release = await acquirePlatformFlagRead();
  });

  test.afterEach(async () => {
    await release?.();
    release = undefined;
  });
}
