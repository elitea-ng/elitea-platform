import type { Dirent } from 'node:fs';
import { mkdtempSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, relative, sep } from 'node:path';

import { describe, expect, it } from 'vitest';

/**
 * The toolkit Run tab must not go back to socket.io (issue 340).
 *
 * # What this pins, and why the existing tests do not
 *
 * `useToolkitChat.hooks.test.tsx` already asserts
 * `getEmitted('chat_predict')).toHaveLength(0)` — but PER SCENARIO, on the one
 * hook it renders. That is the right assertion for a behaviour and the wrong
 * one for a rule: it says nothing about a NEW call site in a component the
 * suite does not mount, and the toolkit feature is 130-odd files. The socket
 * emit is exactly the kind of thing that comes back through a file nobody
 * thought to re-mount.
 *
 * So this reads the source tree instead of running it, the way
 * `src/routes/__tests__/routeWiring.test.ts` and `src/shared/brand/__tests__/
 * reference-scan.ts` do.
 *
 * # The rule is CONFINEMENT, not absence
 *
 * One emit survives and must: `index_data` is the indexing capability, which
 * runs as a durable job over the SSE execution stream and keeps the socket emit
 * as the fallback for a stream that never connects. Every OTHER tool now has a
 * real synchronous REST contract — `POST /elitea_core/test_tool/prompt_lib/
 * {projectId}/{toolId}` through `api/toolkitTestRun.ts` — which is what issue
 * 340 delivered, and which the early return in `startToolRun` routes to before
 * the socket payload is even built.
 *
 * Asserting zero emits would therefore be false today and would have to be
 * waived immediately. Asserting the exact site, and asserting that the REST
 * arm returns BEFORE it, is the rule that actually holds.
 */

const TOOLKIT_ROOTS = ['src/features/toolkits', 'src/pages/toolkits'] as const;

/** The one file allowed to emit, and the tool that reaches the emit. */
const ALLOWED_EMIT_FILE = 'src/features/toolkits/lib/hooks/useToolkitChatDispatch.hooks.ts';

const SKIP_DIRS = new Set(['node_modules', 'dist', 'coverage', '.git', '__tests__', '__mocks__']);
const SOURCE_RE = /\.(ts|tsx)$/;
const TEST_RE = /\.(test|spec)\.(ts|tsx)$/;

/**
 * Any `emit` of `chat_predict`, however the socket client is spelled:
 * `socket.emit('chat_predict'`, `deps.emitSocket("chat_predict"`,
 * `emit(\`chat_predict\``. Keyed on the EMIT, not on the string — the receive
 * listener (`socket.on('chat_predict', …)`) is a different fact and stays
 * wired for the index path.
 */
const EMIT_RE = /emit(?:Socket)?\s*\(\s*['"`]chat_predict['"`]/;

export interface EmitSite {
  readonly file: string;
  readonly line: number;
}

export function scanChatPredictEmits(repoRoot: string, roots: readonly string[]): EmitSite[] {
  const found: EmitSite[] = [];

  const walk = (dir: string): void => {
    let entries: Dirent<string>[];
    try {
      entries = readdirSync(dir, { withFileTypes: true, encoding: 'utf8' });
    } catch {
      return; // A root that does not exist is reported by the caller's own assertion.
    }
    for (const entry of entries) {
      const full = join(dir, entry.name);
      if (entry.isDirectory()) {
        if (!SKIP_DIRS.has(entry.name)) walk(full);
        continue;
      }
      if (!SOURCE_RE.test(entry.name) || TEST_RE.test(entry.name)) continue;
      const source = readFileSync(full, 'utf8');
      if (!source.includes('chat_predict')) continue; // cheap prefilter
      source.split('\n').forEach((text, index) => {
        if (EMIT_RE.test(text)) found.push({ file: relative(repoRoot, full).split(sep).join('/'), line: index + 1 });
      });
    }
  };

  for (const root of roots) walk(join(repoRoot, root));
  return found;
}

const repoRoot = process.cwd();

describe('the toolkit Run tab does not emit chat_predict', () => {
  it('scans a tree that actually exists', () => {
    // The gate's own floor. A scanner pointed at a moved directory finds
    // nothing and reports OK, which is the "absence reads as correctness"
    // failure this repository keeps meeting.
    for (const root of TOOLKIT_ROOTS) {
      const entries = readdirSync(join(repoRoot, root), { withFileTypes: true });
      expect(entries.length, `${root} must exist and hold files`).toBeGreaterThan(0);
    }
  });

  it('confines every socket emit to the index_data fallback', () => {
    const sites = scanChatPredictEmits(repoRoot, TOOLKIT_ROOTS);
    expect(
      sites.map((site) => site.file),
      'a NEW chat_predict emit appeared in the toolkit feature — every tool but index_data runs over POST /elitea_core/test_tool/... (issue 340)',
    ).toEqual([ALLOWED_EMIT_FILE]);
  });

  it('keeps the REST arm in front of the surviving emit', () => {
    const source = readFileSync(join(repoRoot, ALLOWED_EMIT_FILE), 'utf8');
    const lines = source.split('\n');
    const restArm = lines.findIndex((line) => line.includes('tool !== IndexesToolsEnum.indexData'));
    const emit = lines.findIndex((line) => EMIT_RE.test(line));

    expect(restArm, 'the non-index_data early return must still exist').toBeGreaterThan(-1);
    expect(emit, 'the index_data fallback emit must still exist').toBeGreaterThan(-1);
    // The ORDER is the rule. The emit is only unreachable for other tools
    // because the REST arm returns first; move it after and every tool would
    // ride socket.io again while this file still looked correct.
    expect(restArm, 'the REST arm must return BEFORE the socket payload is built').toBeLessThan(emit);
    expect(source, 'the REST call must still be the one the early return makes').toContain('testToolkitTool(');
  });
});

describe('the scanner itself', () => {
  // A rule with no failing case is no rule. These run the scanner over a
  // fixture tree so the gate is proved to go red, rather than being trusted
  // because the real tree happens to pass.
  const withFixture = (files: Readonly<Record<string, string>>, assert: (root: string) => void): void => {
    const root = mkdtempSync(join(tmpdir(), 'chat-predict-scan-'));
    try {
      for (const [path, body] of Object.entries(files)) {
        const full = join(root, path);
        mkdirSync(join(full, '..'), { recursive: true });
        writeFileSync(full, body, 'utf8');
      }
      assert(root);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  };

  it('reports a new emit', () => {
    withFixture({ 'src/features/toolkits/NewRunButton.tsx': "socket.emit('chat_predict', payload);\n" }, (root) => {
      expect(scanChatPredictEmits(root, TOOLKIT_ROOTS)).toEqual([
        { file: 'src/features/toolkits/NewRunButton.tsx', line: 1 },
      ]);
    });
  });

  it('reports an emit spelled through a dependency seam', () => {
    withFixture({ 'src/pages/toolkits/Run.ts': 'const a = 1;\ndeps.emitSocket("chat_predict", body);\n' }, (root) => {
      expect(scanChatPredictEmits(root, TOOLKIT_ROOTS)).toEqual([{ file: 'src/pages/toolkits/Run.ts', line: 2 }]);
    });
  });

  it('does not report the receive listener, a type reference or a comment', () => {
    withFixture(
      {
        'src/features/toolkits/Receive.ts': [
          "socket.on('chat_predict', handle);",
          "type X = ReceivePayloadOf<'chat_predict'>;",
          '// the chat_predict emit used to live here',
        ].join('\n'),
      },
      (root) => {
        expect(scanChatPredictEmits(root, TOOLKIT_ROOTS)).toEqual([]);
      },
    );
  });

  it('does not report a test file, which may assert on the emit', () => {
    withFixture({ 'src/features/toolkits/Run.test.ts': "expect(emitted('chat_predict')).toHaveLength(0);\n" }, (root) => {
      expect(scanChatPredictEmits(root, TOOLKIT_ROOTS)).toEqual([]);
    });
  });
});
