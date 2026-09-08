/**
 * The platform-flag lock's own concurrency rules (`e2e/fixtures/platformFlags.ts`).
 *
 * A Playwright journey cannot state any of these: they are properties of two
 * worker PROCESSES racing over one deployment-wide flag, and the failure they
 * produce lands in whichever unrelated journey happened to be inside the
 * window. The three rules below are exactly the ones the support-widget
 * failure needed — see that fixture's header for the measurement.
 *
 * `scripts` is the only vitest project that collects a file outside `src/`,
 * which is why the lock's test lives here rather than beside it.
 */
import { mkdir, rm, stat, utimes, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { withPlatformFlagLock } from '../e2e/fixtures/platformFlags';

const ROOT = join(tmpdir(), 'elitea-e2e-platform-flags');
const WRITER = join(ROOT, 'writer');

const clean = async () => rm(ROOT, { recursive: true, force: true });

beforeEach(clean);
afterEach(clean);

/** A promise plus the handle that settles it, so a body can be held open. */
function gate() {
  let open;
  const held = new Promise((resolve) => {
    open = resolve;
  });
  return { held, open };
}

describe('the platform-flag window', () => {
  it('lets a sibling of the same group in while the first member is still working', async () => {
    const first = gate();
    const inside = gate();
    const order = [];
    let closed = 0;

    const a = withPlatformFlagLock(
      async () => {
        order.push('a-in');
        inside.open();
        await first.held;
        order.push('a-out');
      },
      { group: 'support', onLastExit: async () => { closed += 1; } },
    );

    // `b` starts only once `a` is demonstrably inside. Started together, the
    // two race for the `mkdir` and either may be the one that opens the
    // window — a coin toss this test must not depend on.
    await inside.held;

    const b = withPlatformFlagLock(
      async () => {
        order.push('b-in');
      },
      { group: 'support', onLastExit: async () => { closed += 1; } },
    );

    // `b` runs to completion while `a` is still inside. Without the shared
    // group this awaits for ever — `a` never leaves until it is released.
    await b;
    expect(order).toEqual(['a-in', 'b-in']);

    // …and the window is still held by `a`, so the flags this group wrote are
    // not restored yet.
    expect(closed).toBe(0);

    first.open();
    await a;
    // Exactly once, by the LAST member out.
    expect(closed).toBe(1);
    await expect(stat(WRITER)).rejects.toThrow();
  });

  it('keeps a caller of another group out until the window closes', async () => {
    const first = gate();
    const inside = gate();
    const order = [];

    const held = withPlatformFlagLock(async () => {
      order.push('held-in');
      inside.open();
      await first.held;
      order.push('held-out');
    }, { group: 'support' });

    await inside.held;

    const other = withPlatformFlagLock(async () => {
      order.push('other-in');
    }, { group: 'branding' });

    // Give the waiter several poll intervals to get it wrong.
    await new Promise((resolve) => setTimeout(resolve, 400));
    expect(order).toEqual(['held-in']);

    first.open();
    await Promise.all([held, other]);
    expect(order).toEqual(['held-in', 'held-out', 'other-in']);
  });

  it('breaks a window whose holder stopped beating, and only that one', async () => {
    // A worker that died mid-window: the token is there, the beat is old.
    await mkdir(WRITER, { recursive: true });
    await writeFile(join(WRITER, 'heartbeat'), 'orphan');
    const old = new Date(Date.now() - 120_000);
    await utimes(join(WRITER, 'heartbeat'), old, old);

    let entered = false;
    // No timeout is asserted here beyond vitest's own: under the wall-clock
    // rule this waited the full bound before breaking in.
    await withPlatformFlagLock(async () => {
      entered = true;
    });
    expect(entered).toBe(true);
  });
});
