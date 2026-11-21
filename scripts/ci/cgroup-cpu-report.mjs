#!/usr/bin/env node
// The cgroup's CPU throttling over a span, from two reads of
// /sys/fs/cgroup/cpu.stat taken before and after it.
//
//   node scripts/ci/cgroup-cpu-report.mjs "$before" "$after"
//
// ci-web-e2e-native.yml prints it after the Playwright run. The native legs
// scale their test timeout for the pool's CPU quota (E2E_TIMEOUT_SCALE), and
// this line keeps that reason measured. It never fails the step: an
// unreadable cpu.stat (a host without cgroup v2) prints that, and exits 0.
import { readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';

/** `cpu.stat` text → { key: number }. */
export function parseCpuStat(text) {
  const out = {};
  for (const line of String(text ?? '').split('\n')) {
    const [key, value] = line.trim().split(/\s+/);
    if (key && value !== undefined && Number.isFinite(Number(value))) out[key] = Number(value);
  }
  return out;
}

/** The report line for two `cpu.stat` reads; `cpuMax` is cpu.max's text. */
export function cpuReport(beforeText, afterText, cpuMax = '') {
  const before = parseCpuStat(beforeText);
  const after = parseCpuStat(afterText);
  if (!('nr_periods' in after)) return 'cgroup cpu.stat: not readable';
  const delta = (key) => (after[key] ?? 0) - (before[key] ?? 0);
  const periods = delta('nr_periods');
  const throttled = delta('nr_throttled');
  const pct = periods ? ((100 * throttled) / periods).toFixed(1) : '0.0';
  const seconds = (delta('throttled_usec') / 1e6).toFixed(1);
  const max = cpuMax.trim() ? `, cpu.max ${cpuMax.trim()}` : '';
  return `cgroup CPU over the run: ${throttled} of ${periods} periods throttled (${pct}%), ${seconds} s throttled${max}`;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  let cpuMax = '';
  try {
    cpuMax = readFileSync('/sys/fs/cgroup/cpu.max', 'utf8');
  } catch {
    cpuMax = '';
  }
  console.log(cpuReport(process.argv[2], process.argv[3], cpuMax));
}
