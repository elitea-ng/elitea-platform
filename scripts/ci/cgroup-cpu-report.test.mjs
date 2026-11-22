import { test } from 'node:test';
import assert from 'node:assert/strict';
import { cpuReport, parseCpuStat } from './cgroup-cpu-report.mjs';

const BEFORE = 'usage_usec 1\nnr_periods 100\nnr_throttled 10\nthrottled_usec 1000000\n';
const AFTER = 'usage_usec 9\nnr_periods 1100\nnr_throttled 410\nthrottled_usec 61000000\n';

test('parses cpu.stat into numbers', () => {
  assert.deepEqual(parseCpuStat(BEFORE), { usage_usec: 1, nr_periods: 100, nr_throttled: 10, throttled_usec: 1000000 });
  assert.deepEqual(parseCpuStat(''), {});
  assert.deepEqual(parseCpuStat(undefined), {});
});

test('reports the throttling between two reads', () => {
  assert.equal(
    cpuReport(BEFORE, AFTER, '400000 100000\n'),
    'cgroup CPU over the run: 400 of 1000 periods throttled (40.0%), 60.0 s throttled, cpu.max 400000 100000',
  );
  assert.equal(
    cpuReport(BEFORE, BEFORE),
    'cgroup CPU over the run: 0 of 0 periods throttled (0.0%), 0.0 s throttled',
  );
});

test('an unreadable cpu.stat says so instead of reporting zeros', () => {
  assert.equal(cpuReport('', ''), 'cgroup cpu.stat: not readable');
  assert.equal(cpuReport(BEFORE, ''), 'cgroup cpu.stat: not readable');
});
