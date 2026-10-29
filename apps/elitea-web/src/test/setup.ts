import { afterAll, afterEach, beforeAll } from 'vitest';
import { setupServer } from 'msw/node';
import { cleanup, configure } from '@testing-library/react';

// [S1] `toBeInTheDocument`/`toHaveTextContent`/etc. — every `shared/ui`
// component test in this unit asserts against the rendered DOM via these
// matchers. `@testing-library/jest-dom/vitest` self-registers with vitest's
// `expect` on import (no `expect.extend` call needed — verified against the
// installed package's `./vitest` export, `@testing-library/jest-dom@6.9.1`).
// Global, additive, and required by ANY future DOM-assertion test in the
// tree, not just this unit's — so it belongs in the shared bootstrap F4
// authored rather than being re-imported per test file.
import '@testing-library/jest-dom/vitest';

// [M1 carry-forward] Node 24 ships an experimental `localStorage` global that
// shadows jsdom's and resolves to `undefined` without `--localstorage-file`,
// so `window.localStorage` is undefined in the `node` project and any
// component reading it during an effect throws (`<Sidebar>`, `<AppShell>`).
// Installed here — not per test file — so every unit inherits it; see the
// shim's own module comment for the full diagnosis. No-ops when the
// environment already provides working storage.
import { installWebStorageShim } from '@/shared/lib/webstorage.testshim';

import { handlers } from './msw/handlers/index';

// `findBy*`/`waitFor` give up after 1 s by default. CI runs this suite in five
// shards on 4-vCPU runners with V8 coverage on, which makes a render several
// times slower than on a laptop, so a correct test that waits for a lazy chunk
// or a chained query timed out on whichever file the shard split happened to
// load heavily (#1531 hit two different files on consecutive runs). One
// generous ceiling here keeps that out of individual tests: a passing wait
// still returns as soon as its assertion holds, and only a genuinely failing
// one takes longer to report.
configure({ asyncUtilTimeout: 5_000 });

const shimmedStorages = installWebStorageShim();

/**
 * jsdom ships no `ResizeObserver`, and several components create one
 * unconditionally on mount (`useTextOverflow` via
 * `TypographyWithConditionalTooltip`, reached from any tree containing
 * `EllipsisTypography`). Individual test files used to stub it themselves with
 * `vi.stubGlobal`, which leaks across files in the same worker: a test that
 * needed the stub but did not install it passed locally — because a file that
 * DID install it happened to run first in that worker — and failed in CI, where
 * the shard split put it in a worker on its own. `EditApplication.test.tsx` hit
 * exactly that after it began rendering `CreateAgentForm`.
 *
 * Installing it here makes the environment deterministic instead of dependent
 * on file ordering. Per-file `vi.stubGlobal` calls still override this.
 */
if (!('ResizeObserver' in globalThis)) {
  (globalThis as unknown as { ResizeObserver: unknown }).ResizeObserver = class {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  };
}

/**
 * jsdom implements the SVG DOM but none of its GEOMETRY: `getBBox`,
 * `getComputedTextLength` and `getScreenCTM` are all absent, because jsdom has
 * no layout engine to answer them. Any component that measures rendered SVG
 * throws `getBBox is not a function` on mount — `shared/ui/MermaidDiagram` does,
 * because mermaid measures every label to lay a diagram out.
 *
 * Installed here for the same reason as `ResizeObserver` above: this is a
 * browser API jsdom lacks, not application behaviour, so stubbing it is the
 * environment's job and not a test's (`elitea/no-vi-mock`, R-M1, bans reaching
 * for `vi.mock` to paper over it). The numbers are a fixed, non-zero box — real
 * layout is what a browser gives you, and the `storybook` vitest project runs in
 * a real Chromium when that matters. What these polyfills buy the `node`
 * project is the ability to tell "rendered" apart from "threw", which is exactly
 * what the MermaidDiagram tests assert.
 */
//
// GUARDED on SVGElement existing. This file is the setup for the `node` (jsdom)
// project, but it is ALSO booted by the rule-layer self-test fixtures
// (scripts/selftest/vitest.fixtures.config.mts), which run without a DOM. There
// `globalThis.SVGElement` is undefined, and reading `.prototype` off it threw
// during setup — so every fixture collected ZERO tests and the R-M3/R-M5 rules
// reported their passing fixture as failing. A setup file that throws does not
// fail one test; it silently empties the suite.
if (typeof globalThis.SVGElement !== 'undefined') {
  const svgProto = globalThis.SVGElement.prototype as unknown as Record<string, unknown>;
  svgProto['getBBox'] ??= function getBBox() {
    return { x: 0, y: 0, width: 100, height: 20 };
  };
  svgProto['getComputedTextLength'] ??= function getComputedTextLength() {
    return 100;
  };
  svgProto['getScreenCTM'] ??= function getScreenCTM() {
    const identity = { a: 1, b: 0, c: 0, d: 1, e: 0, f: 0 };
    return { ...identity, inverse: () => identity };
  };
}

/**
 * Global test bootstrap for the `node` (jsdom) vitest project (spec §6.3).
 *
 * Mocks stop at the network boundary (§6.2): the ONLY substitutions a test
 * may make are this MSW server and the socket in-memory double (unit S5).
 *
 * R-M5 (§6.5): `onUnhandledRequest: 'error'` — a request no handler covers
 * fails the test instead of silently hitting the network. Proven by the
 * msw fixture pair in scripts/check-gates-selftest.mjs.
 */
export const server = setupServer(...handlers);

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' });
});

afterEach(() => {
  // Removes runtime handlers added via server.use() so network behaviour
  // never leaks between tests.
  server.resetHandlers();

  // Storage outlives a test either way: the shim above installs ONE instance
  // for the whole worker, and a working environment storage (what vitest 5's
  // jsdom provides, so the shim no-ops) lives as long as the file's window.
  // Without this reset, storage written by one test is visible to every later
  // test — an isolation leak that shows up as tests passing alone and failing
  // in-suite (WorkspaceComposer's composer drafts did, on the vitest 5 bump).
  // So clear BOTH stores, whoever provides them, not only the shimmed ones.
  //
  // Unmount first. A component may write storage as it unmounts (the composer
  // saves its draft then), and testing-library's own unmount hook is an
  // afterEach whose order against this one depends on import order: importing
  // `@testing-library/react` here, for `configure`, put it after this clear,
  // and every WorkspaceComposer test started with the previous test's draft.
  // `cleanup()` is idempotent, so its own hook running later is harmless.
  cleanup();
  for (const name of new Set<string>([...shimmedStorages, 'localStorage', 'sessionStorage'])) {
    try {
      (globalThis as unknown as Record<string, Storage | undefined>)[name]?.clear();
    } catch {
      // An environment whose storage accessor throws has nothing to leak.
    }
  }
});

afterAll(() => {
  server.close();
});
