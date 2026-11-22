/**
 * [#coverage-pool] Per-test time budgets, scaled for coverage runs.
 *
 * Coverage instrumentation makes this suite 2-3x slower on the self-hosted
 * 4-vCPU pool, so a budget sized for a plain run times out a slow-but-correct
 * test there. vitest.config.ts works out one multiplier for the run (3 under
 * `--coverage`, 1 otherwise, or `VITEST_TIMEOUT_SCALE` when it is set) and
 * hands it to the workers as `VITEST_TIMEOUT_SCALE`. Every explicit budget in a
 * test file (`it(..., testTimeout(30_000))`, `{ timeout: testTimeout(N) }`,
 * `vi.setConfig({ testTimeout: testTimeout(N) })`) and every testing-library
 * `asyncUtilTimeout` goes through here, so it keeps its ratio to the
 * config-level `testTimeout`. Outside coverage the scale is 1 and nothing
 * changes.
 *
 * Waits passed to one `findBy*`/`waitFor` call are left alone: they return as
 * soon as their assertion holds and are not budgets.
 */

/** Parses a scale value. Anything that is not a finite number of at least 1 reads as 1. */
export function parseTimeoutScale(raw: unknown): number {
  if (typeof raw !== 'string' || raw.trim() === '') return 1;
  const scale = Number(raw);
  return Number.isFinite(scale) && scale >= 1 ? scale : 1;
}

/** `ms` multiplied by this run's scale (`VITEST_TIMEOUT_SCALE`, default 1). */
export function testTimeout(ms: number): number {
  return Math.round(ms * parseTimeoutScale(import.meta.env.VITEST_TIMEOUT_SCALE));
}
