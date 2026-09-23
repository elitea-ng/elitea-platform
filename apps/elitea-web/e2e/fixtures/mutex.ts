/**
 * A cross-worker mutex over one named piece of SHARED SERVER STATE.
 *
 * ## Why this exists
 *
 * `fullyParallel` is on and every journey works against one deployment. Most
 * journeys create entities of their own and never meet; a few act on state
 * there is exactly ONE of — a project's Project Context row, a
 * deployment-wide sweep with no "only my rows" form. Two of those interleaved
 * is a CLOBBER, not a flake a retry fixes: the value one test polls for is
 * overwritten by another test's write, and the failure reads exactly like the
 * feature being broken.
 *
 * `platformFlags.ts` solved the same problem for the platform-flag rows with a
 * writer/reader window bound to one fixed path. This is the smaller shape: one
 * exclusive holder per NAME, so two unrelated pieces of shared state never
 * queue behind each other.
 *
 * ## The mechanism
 *
 * `mkdir` of a directory, because it fails atomically on an existing one
 * everywhere and needs no dependency.
 *
 * The heartbeat is not decoration. A holder touches `heartbeat` every
 * `BEAT_MS`; a waiter only breaks a token whose beat has STOPPED. A
 * stale-timestamp-only lock cannot tell a live holder from a dead worker and
 * eventually evicts a test that is still running — measured in
 * `platformFlags.ts`, where a live holder's window was broken by the third
 * sibling to queue and the victim's assertion failed as "the widget never
 * rendered".
 *
 * Nothing here can wedge a run: a waiter gives up at a far outer bound and
 * breaks the token, because a lock that can stop CI is worse than the race.
 */
import { randomUUID } from 'node:crypto';
import { mkdir, rm, stat, utimes, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const ROOT = join(tmpdir(), 'elitea-e2e-named-locks');

/** How often a holder proves it is still there. */
const BEAT_MS = 2_000;
/** A window whose beat is older than this belongs to a worker that is gone. */
const HEARTBEAT_STALE_MS = 20_000;
/**
 * The outer bound on waiting, after which the waiter breaks the token. Well
 * past any honest window, and present only so an unforeseen shape cannot wedge
 * a run.
 */
const WAIT_LIMIT_MS = 240_000;
const POLL_MS = 100;

const sleep = async (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms));

/** Names must be filesystem-safe: they become a directory. */
function holderPath(name: string): string {
  return join(ROOT, name.replace(/[^a-z0-9-]/gi, '_'));
}

/** How long ago the window last proved it was alive; `undefined` when there is none. */
async function beatAge(holder: string): Promise<number | undefined> {
  try {
    const beat = await stat(join(holder, 'heartbeat'));
    return Date.now() - beat.mtimeMs;
  } catch {
    try {
      // The directory exists but the beat file does not — the instant between
      // `mkdir` and the first write. Treat it as a fresh window.
      await stat(holder);
      return 0;
    } catch {
      return undefined;
    }
  }
}

/** True while a live holder is inside. Breaks an orphaned token on the way. */
async function held(holder: string): Promise<boolean> {
  const age = await beatAge(holder);
  if (age === undefined) return false;
  if (age > HEARTBEAT_STALE_MS) {
    await rm(holder, { recursive: true, force: true });
    return false;
  }
  return true;
}

/**
 * Takes the named window and returns its release.
 *
 * Call it in `beforeEach` and release in `afterEach`, so a test that throws
 * mid-way still frees the state for the next one.
 */
export async function acquireNamedLock(name: string): Promise<() => Promise<void>> {
  await mkdir(ROOT, { recursive: true });
  const holder = holderPath(name);
  const beatFile = join(holder, 'heartbeat');
  const deadline = Date.now() + WAIT_LIMIT_MS;
  const owner = `${String(process.pid)}-${randomUUID()}`;

  for (;;) {
    try {
      await mkdir(holder);
      break;
    } catch {
      // Also breaks an orphaned window, so this cannot spin for ever.
      await held(holder);
      if (Date.now() > deadline) {
        await rm(holder, { recursive: true, force: true });
        continue;
      }
      await sleep(POLL_MS);
    }
  }
  await writeFile(beatFile, owner);
  const beat = setInterval(() => {
    const now = new Date();
    void utimes(beatFile, now, now).catch(() => {
      /* the token was broken under us; the release below is still safe */
    });
  }, BEAT_MS);

  return async () => {
    clearInterval(beat);
    await rm(holder, { recursive: true, force: true });
  };
}
