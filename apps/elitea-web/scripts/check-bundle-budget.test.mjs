import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { afterEach, describe, expect, it } from 'vitest';

/**
 * `chunkLimits` in bundle-budget.json: a per-chunk allowance matched by
 * file-name prefix, applied before `routeChunkKB`. The script runs as a child
 * process against a synthetic dist/ so no real build is needed.
 */
const SCRIPT = fileURLToPath(new URL('./check-bundle-budget.mjs', import.meta.url));

let workspace = null;
afterEach(() => {
  if (workspace) rmSync(workspace, { recursive: true, force: true });
  workspace = null;
});

/** Incompressible-ish filler so the gzip size is close to `kib`. */
function filler(kib) {
  let state = 0x2545f491;
  const bytes = Buffer.alloc(kib * 1024);
  for (let i = 0; i < bytes.length; i++) {
    state ^= state << 13;
    state ^= state >>> 17;
    state ^= state << 5;
    bytes[i] = state & 0xff;
  }
  return bytes;
}

function run({ chunks, chunkLimits }) {
  workspace = mkdtempSync(path.join(tmpdir(), 'budget-'));
  const root = path.join(workspace, 'dist', 'app');
  mkdirSync(path.join(root, 'assets'), { recursive: true });
  writeFileSync(path.join(root, 'index.html'), '<script type="module" src="/assets/entry.js"></script>');
  writeFileSync(path.join(root, 'assets', 'entry.js'), 'export {};');
  for (const [name, kib] of Object.entries(chunks)) writeFileSync(path.join(root, 'assets', name), filler(kib));
  const target = { initialKB: 100, routeChunkKB: 10 };
  if (chunkLimits) target.chunkLimits = chunkLimits;
  const budget = path.join(workspace, 'budget.json');
  writeFileSync(budget, JSON.stringify({ targets: { app: target } }));
  return spawnSync('node', [SCRIPT, '--dist', path.join(workspace, 'dist'), '--budget', budget], { encoding: 'utf8' });
}

describe('check-bundle-budget chunkLimits', () => {
  it('lets a named chunk exceed routeChunkKB up to its own limit', () => {
    const result = run({ chunks: { 'elk-AAAA-bbb.js': 40 }, chunkLimits: { elk: { kb: 60 } } });
    expect(result.status).toBe(0);
  });

  it('fails a named chunk over its own limit', () => {
    const result = run({ chunks: { 'elk-AAAA-bbb.js': 40 }, chunkLimits: { elk: { kb: 20 } } });
    expect(result.status).toBe(1);
    expect(result.stderr).toContain('over the 20 KiB budget');
  });

  it('still holds every other chunk to routeChunkKB', () => {
    const result = run({
      chunks: { 'elk-AAAA-bbb.js': 40, 'other-CCCC.js': 40 },
      chunkLimits: { elk: { kb: 60 } },
    });
    expect(result.status).toBe(1);
    expect(result.stderr).toContain('other-CCCC.js');
  });

  it('fails when an entry matches no emitted chunk', () => {
    const result = run({ chunks: { 'small-DDDD.js': 1 }, chunkLimits: { elk: { kb: 60 } } });
    expect(result.status).toBe(1);
    expect(result.stderr).toContain('matches no emitted chunk');
  });
});
