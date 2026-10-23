// vitest.config.ts — spec §6.3 VERBATIM (unit F2). Deviations are marked
// [F2] with their reason; everything else must stay byte-compatible with the
// spec so V4's coverage-shape audit can diff it.
import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';
import tsconfigPaths from 'vite-tsconfig-paths';
// [S2] vite.config.ts registers `svgr({ svgrOptions: { ref: true } })` for the app build, but
// vitest has its own separate plugin list — without this, `*.svg?react` imports (every icon in
// shared/ui/icons/**) fall through to Vite's default asset handling under jsdom and resolve to a
// `data:image/svg+xml,...` URL string instead of a component, breaking every icon-consuming test
// tree-wide (proven: shared/ui/icons' own smoke test failed with `InvalidCharacterError` before
// this was added). `svgrOptions` must match vite.config.ts's so dev/build/test agree on the
// generated component shape (ref-forwarding).
import svgr from 'vite-plugin-svgr';
// [F2] vitest@4.1.10 rejects the string form the spec shows
// (`provider: 'playwright'`): "The `browser.provider` configuration was
// changed to accept a factory instead of a string." — verified 2026-07-26.
import { playwright } from '@vitest/browser-playwright';
// [S1] activates the storybook project (§6.4): addon-vitest's plugin turns
// every *.stories.tsx CSF file into a Vitest test per story, running the
// `play` function and the a11y check registered in .storybook/preview.tsx.
import { storybookTest } from '@storybook/addon-vitest/vitest-plugin';
// [client-core] The same dev-server `fs.allow` as the app: @elitea/client-core
// lives outside this app's root, and the storybook project's browser-mode
// server would otherwise refuse to serve it. Applied at the root so every
// project (`extends: true`) inherits it.
import { withClientCoreAccess } from './vite.config';

export default defineConfig(withClientCoreAccess({
  plugins: [react(), tsconfigPaths(), svgr({ svgrOptions: { ref: true } })],
  test: {
    projects: [
      {
        extends: true,
        test: {
          name: 'node',
          environment: 'jsdom',
          globals: true,
          setupFiles: ['./src/test/setup.ts'],
          include: ['src/**/*.{test,spec}.{ts,tsx}'],
        },
      },
      // [#424] The `browser` project that used to sit here is GONE. No CI job
      // ever selected it: ci-web.yml runs `--project node` and
      // `--project storybook` only. There are no `*.browser.test.*` files
      // today, so the first one somebody wrote would have run nowhere, and the
      // `node` project excluded that same pattern, so it would not have run
      // there either. Both halves are removed together: a `*.browser.test.ts`
      // added now is collected by the `node` project above and runs.
      //
      // Browser-mode coverage has not been lost. The `storybook` project below
      // runs in a real Chromium through the same @vitest/browser-playwright
      // factory. If a browser-mode unit-test project is wanted again, it
      // arrives with the CI job that selects it.
      // [S1] Storybook project (§6.3/§6.4): activated now that `.storybook/`
      // (main.ts + preview.tsx + vitest.setup.ts) has landed. Runs every
      // `shared/ui/**/*.stories.tsx` as a Vitest test, executing each story's
      // `play` function and the a11y addon's check (parameters.a11y.test is
      // 'error' in preview.tsx). Browser mode uses the
      // `@vitest/browser-playwright` factory
      // (D2 addendum: the string form `provider: 'playwright'` is rejected).
      {
        extends: true,
        plugins: [storybookTest({ configDir: '.storybook' })],
        test: {
          name: 'storybook',
          browser: {
            enabled: true,
            provider: playwright(),
            // Real CI gap, found the first time this project ran end-to-end
            // in CI (draft-PR CI hadn't executed before unit S1 landed):
            // headless Chromium denies clipboard-write permission by
            // default, unlike a normal user's browser. CopyToClipboardButton
            // and CommonStringField's clipboard variant both, deliberately
            // and correctly (see src/shared/lib/clipboard.ts's own N4 doc
            // comment — a byte-for-byte preserved old-app quirk, not
            // something to fix here), fire an unhandled, unawaited
            // navigator.clipboard.writeText() retry when EVERY copy path
            // fails. Without this permission grant, that retry always fails
            // too, in an environment these stories were never meant to
            // simulate. Granting the permission (matching what a real
            // browser has) keeps the primary clipboard write succeeding, so
            // the preserved-quirk fallback path never triggers in normal
            // Storybook test runs — the same class of fix already applied
            // to ci-web.yml for the Playwright browser install itself.
            instances: [{ browser: 'chromium', contextOptions: { permissions: ['clipboard-read', 'clipboard-write'] } }],
          },
          setupFiles: ['./.storybook/vitest.setup.ts'],
        },
      },
      // [F2] Extra project, not in §6.3: the gate scripts' own test suites
      // (§9.3 unit F2 requires 100% coverage of scripts' decision logic; the
      // spec's include only matches src/**). The `coverage` block below is
      // untouched by this project — the 100% floor on scripts/lib/** is
      // enforced by the "Gate-script decision-logic coverage" job in
      // ci-web.yml, which re-runs this project with a CLI-scoped
      // --coverage.include and 100% thresholds. That job's thresholds were 0
      // and its report reader was broken until #73; the floor is real now.
      {
        extends: true,
        test: {
          name: 'scripts',
          environment: 'node',
          include: ['scripts/**/*.test.mjs', 'tools/lint-rules/**/*.test.mjs'],
        },
      },
    ],
    coverage: {
      provider: 'v8',
      reporter: ['text-summary', 'json-summary', 'lcov', 'html'],
      reportsDirectory: './coverage',
      all: true,
      include: ['src/**/*.{ts,tsx}'],
      exclude: [
        'src/**/*.stories.tsx',
        'src/**/*.d.ts',
        // orval writes its hook and schema modules into the per-tag
        // SUBDIRECTORIES of src/shared/api/generated/ (admin/, chat/, …).
        // Those stay excluded. The contract tests cover generated code, not
        // line coverage.
        //
        // The glob was `src/shared/api/generated/**`. That form also waived
        // the one hand-written file in the tree: `generated/mutator.ts`, the
        // `eliteaFetch` adapter that every generated hook calls. See its own
        // header, and mutator.test.ts's "carries the ≥90% infra floor" note.
        // ci-web.yml's gate-mutator-coverage job reads that floor out of the
        // merged coverage report. The old glob made the job unable to see a
        // number. The job took its "not in coverage — skipping" exit on every
        // run it ever had (issue #421, item 2).
        'src/shared/api/generated/*/**',
        // `src/shared/api/sse.ts` used to be excluded here. It was removed
        // (issue #309, Gate 3) and removing it excluded nothing further: the
        // module became the DIRECTORY src/shared/api/sse/, so the glob had
        // stopped matching any file at all — an exclusion that reads as a live
        // waiver while covering nothing, which is worse than either state.
        // scripts/check-ci-dormancy.mjs now fails on a zero-match exclusion.
        // Deliberately unwired (knip.json's ignoreFiles has the same entry,
        // same reason): its target library (@mui/x-treeview) is not a
        // dependency of this app (spec §2.2/P1, "the file tree is hand-
        // rolled") -- REMOVE this line if that ever changes and something
        // actually wires it into mui-overrides/index.ts.
        'src/shared/brand/mui-overrides/MuiTreeItem.ts',
        // Wave-2 C4 chat-messages unit. The original reason for this line —
        // "not wired into any app consumer yet" — IS NO LONGER TRUE, and had
        // not been for some time before issue #309 found it: ten modules now
        // import it, among them processes/chat/ui/ChatWithEditors.hooks.ts and
        // widgets/chat-box/**, i.e. the live chat message layer.
        //
        // It stays excluded for now because the unit is 69 files with 8 test
        // files, and measuring it would drop the merged run under the 80% gate
        // on its first PR — but it stays excluded as a DECLARED debt, not as a
        // comment: scripts/check-ci-dormancy.mjs requires this entry to be
        // listed in coverage-exclusions.json with an owner, an issue and a
        // review-by date, and fails the build once that date passes. The
        // stale-comment version of this waiver survived precisely because
        // nothing could tell it had gone stale.
        'src/features/chat-messages/**',
        'src/test/**',
        // `src/**/__mocks__/**` used to sit here. It matched no file either —
        // this app has no __mocks__ directory anywhere; test doubles live under
        // src/test/msw. Found by scripts/check-ci-dormancy.mjs on its first
        // run, not by issue #309, which is the point of having the rule rather
        // than a list of known-bad entries.
        'src/app/main.tsx',
        'src/routeTree.gen.ts',
      ],
      // Only apply coverage thresholds in non-sharded, merged runs. Sharded
      // coverage runs set VITEST_SKIP_COVERAGE_THRESHOLDS=true so raw shard
      // data is collected without failing on config-level per-glob thresholds.
      thresholds: process.env.VITEST_SKIP_COVERAGE_THRESHOLDS === 'true' ? {} : {
        lines: 80,
        statements: 80,
        functions: 75,
        branches: 70,
        autoUpdate: false,
      },
    },
  },
}));
