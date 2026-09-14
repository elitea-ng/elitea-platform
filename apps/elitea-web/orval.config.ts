/**
 * orval.config.ts — the Channel-A generator (unit S4; spec §5.2/§5.3).
 *
 * Starting point is spec §5.3's config, shown VERBATIM there. Two deviations
 * from that snippet, both forced by facts orval 8.23.0 itself disagrees with
 * the spec's prose about (verified via context7 against the installed
 * version — not guessed; see the S4 report for the full diff):
 *
 *  1. `override.mutator.name` is `eliteaFetch`, but `path` points at
 *     `src/shared/api/generated/mutator.ts` (this unit's own adapter), NOT
 *     `src/shared/api/http.ts` as shown. F4's `http.ts` exports a factory
 *     (`createHttpClient(cfg): HttpClient`), never a function literally
 *     named `eliteaFetch` — there is nothing at that path for orval to
 *     import. `mutator.ts` supplies the named export and wraps F4's client.
 *  2. `output.mock` is NOT the spec's `{ type: 'msw', delay: 0, useExamples:
 *     false }` shape — that is orval's pre-8.0 schema. 8.23.0 moved
 *     per-generator options into `generators: [...]` (orval's own v8
 *     migration guide documents exactly this rename). The verbatim spec
 *     snippet would be silently coerced to a no-op — kept here in the
 *     current schema so mock generation actually runs. It is enabled AS A
 *     BYPRODUCT — S4 does not consume it — because the spec's shown config
 *     asks for it; per spec §5.2 Channel B, unit M1 owns turning these
 *     skeletons into fixture-backed, zod-validated handlers (R-M2/R-M3).
 *     The output lands in `src/shared/api/generated/**\/*.msw.ts` — M1's
 *     input, not S4's.
 *  3. `override.zod.generate` is NOT the spec's bare `true` — orval 8.23.0's
 *     `ZodOptions['generate']` (`@orval/core`'s `.d.mts`, read directly
 *     rather than assumed) is typed `{ param?, query?, header?, body?,
 *     response? }`, never a boolean. Passing `true` there is silently
 *     absorbed as "none of these are objects with a `.response` key" and
 *     orval emits ZERO `.zod.ts` files — reproduced empirically (a clean
 *     `npx orval` with the verbatim spec value produced 0 zod schemas across
 *     78 operations). The five sub-flags are spelled out explicitly instead.
 *  4. `output.schemas` is an OBJECT (`{ path, type: 'zod' }`), not the
 *     spec's bare path string — `output.schemas.type === 'zod'` is what
 *     `@orval/query`'s generator branches on (confirmed by reading
 *     `node_modules/@orval/query/dist/index.mjs`'s `isZodOutput` check) to
 *     produce a zod object + `zod.input<>`/`zod.output<>` type pair for
 *     every named `#/components/schemas/*`. Without it, `override.zod.*`
 *     has nothing to attach to and nothing is emitted at all.
 *
 *     `override.zod.generate.{param,query,header,body,response}` is a
 *     SEPARATE, ADDITIVE control, and it is NOT inert (an earlier version of
 *     this comment claimed it was, based on an A/B test that — unnoticed at
 *     the time — also varied `schemas.type`, so both runs produced zero zod
 *     files regardless of the `generate` flags, making them look identical
 *     for the wrong reason; corrected 2026-07-27 after a properly isolated
 *     re-run holding `schemas.type: 'zod'` and `generateReusableSchemas`
 *     fixed and toggling only `generate.*`). With `schemas.type: 'zod'`
 *     held constant, `generate: {...all true}` vs `{...all false}` produces
 *     152 vs 128 output files — a real 24-file difference, including
 *     `artifactListParams.zod.ts`, `createArtifactBody.zod.ts`,
 *     `listApplicationsParams.zod.ts` and 21 others that the generated
 *     hooks actually `import`. Turning `generate.*` off does NOT fall back
 *     to a plain TS type for those 24 — the file is simply never written,
 *     so the operation file's import dangles (verified: e.g. with
 *     `query: false`, `applications.ts` still `import`s
 *     `ListApplicationsParams` from `"../model"`, and no
 *     `listApplicationsParams.ts`/`.zod.ts` exists anywhere in that
 *     output). DO NOT remove or "simplify away" this block — doing so
 *     deletes real, load-bearing schema files.
 *
 *     KNOWN GAP, reported honestly rather than hidden, and NOT fixed by
 *     `generate: {...all true}` (this repo's actual setting): 3 of 78
 *     operations (`getAnalyticsUserDetail`, `getAnalyticsToolDetail`,
 *     `getAnalyticsAgentDetail`) have a `oneOf: [$ref: A, $ref: B]`
 *     response — e.g. `oneOf: [$ref: AnalyticsDetailEnvelope, $ref:
 *     AnalyticsUsersList]` — where BOTH branches are real named components
 *     that already have their own correct `model/*.zod.ts` file. The gap is
 *     that orval's generator never synthesizes a `zod.union([...])` (or
 *     `zod.discriminatedUnion`) combining oneOf-referenced schemas for the
 *     response as a whole — it falls back to a plain
 *     `export type ...Response200 = { data: AnalyticsDetailEnvelope |
 *     AnalyticsUsersList; status: 200 }` TS alias in the operation file
 *     instead. (A previous version of this comment wrongly described these
 *     three as having an "inline, non-`$ref`" response needing a new named
 *     component schema from W2 — that diagnosis was wrong on inspection of
 *     the actual `v2.yaml` responses and would have pointed a future fix at
 *     the wrong place.) A real fix is a oneOf-aware backfill step
 *     synthesizing the union — legitimate future work, not required for
 *     this unit; not implemented here. 5 more operations are genuinely void
 *     (204/205) and correctly have no response schema at all. See the S4
 *     report's zod-coverage table for the full 78-way breakdown.
 *  5. `hooks.afterAllFilesWrite` runs a backfill step (BEFORE prettier, so
 *     its output gets formatted in the same pass) that `scripts/lib/
 *     orval-zod-backfill.mjs` owns: even with `generate: {...all true}`,
 *     13 symbols still never get a model file — 6 named
 *     `#/components/responses/*` entries (this spec's shared
 *     400/401/403/404/409/500 error responses, sanitised `N400Response`
 *     etc.) and 7 per-operation query-param combiner types (`<Op>Params`)
 *     that combine parameters shared/`$ref`'d across MULTIPLE operations
 *     (`Limit`, `Offset`, `DateFrom`, `DateTo`, `Search`, `SortBy`,
 *     `SortOrder` under `#/components/parameters/*`), plus one
 *     (`GetBrandingBootstrapParams`) whose root cause traces to the same
 *     operation's non-JSON (`application/javascript`) response rather than
 *     its (purely inline) param shape. Purely-inline, single-operation
 *     Params/Body types (e.g. `ListApplicationsParams`,
 *     `GetRecommendationsParams`) DO generate correctly under
 *     `generate: {...all true}` — the residual 13-symbol gap this backfill
 *     covers is narrower than "Params combiners" in general. Without this
 *     step, `tsc --noEmit` fails on those 13 dangling imports. The backfill
 *     computes candidates straight from `v2.yaml` and only writes what
 *     orval didn't already write, so it self-corrects if a future orval
 *     release closes part of the gap.
 *
 * `input.filters` is deliberately omitted (the spec's shown snippet has an
 * empty `{ schemas: [], tags: [] }` with a "manifest-driven; see
 * scripts/filter-spec.ts" comment). That script was never built and isn't
 * needed: unit W2 already scoped `v2.yaml` down to exactly the
 * endpoints.manifest.json set at the SPEC level (78 operations, field-level,
 * zero `Struct`/`GenericResponse` shells — confirmed by
 * `grep -c 'operationId:' v2.yaml` = 78). Filtering an already-scoped file
 * down to itself is a no-op; omitting `filters` avoids the open question of
 * whether an empty-array filter with no `mode` means "exclude nothing" or
 * "match nothing" in 8.23.0 (untested — no reason to depend on it).
 */
import path from 'node:path';
import process from 'node:process';

import { defineConfig } from 'orval';

import { backfillMissingZodModels } from './scripts/lib/orval-zod-backfill.mjs';

/**
 * Where orval writes. `ORVAL_OUT_DIR` redirects the whole generator into a
 * scratch directory; the default is the committed location.
 *
 * `scripts/check-generated-client.mjs` (issue #592) sets it. The gate used to
 * regenerate ON TOP of the committed tree and read `git status`, which cannot
 * see an ORPHAN — a file orval has stopped producing stays on disk, the
 * rewrite never touches it, and the diff stays clean. The gate now regenerates
 * into an EMPTY directory and compares both trees, so a file that is present
 * in the checkout and absent from the run is a failure.
 *
 * The override is one variable, not a second config, on purpose: the check
 * must exercise the SAME generator settings the committed client came from. A
 * copied config would drift, and the gate would then compare the tree against
 * a generator nobody uses.
 *
 * Keep every path below inside `OUT_DIR`. The generated files import the
 * mutator by a path RELATIVE to themselves (`../mutator`), so the mutator must
 * sit at the same depth in the scratch tree; the check copies the hand-written
 * `mutator.ts` in before it runs orval.
 */
const OUT_DIR = process.env.ORVAL_OUT_DIR ?? path.join('src', 'shared', 'api', 'generated');

export default defineConfig({
  elitea: {
    input: {
      target: '../../services/elitea-main/api/openapi/v2.yaml',
    },
    output: {
      mode: 'tags-split',
      target: path.join(OUT_DIR, 'endpoints.ts'),
      schemas: { path: path.join(OUT_DIR, 'model'), type: 'zod' },
      client: 'react-query',
      httpClient: 'fetch',
      baseUrl: '', // resolved at runtime by shared/api/http.ts via shared/config (§7.1)
      mock: {
        generators: [{ type: 'msw', delay: 0, useExamples: false }],
      },
      override: {
        mutator: { path: path.join(OUT_DIR, 'mutator.ts'), name: 'eliteaFetch' },
        operations: {
          // The 200 result and 202 receipt have different required fields.
          // Use a complete default response; recovery tests provide explicit 202 fixtures.
          getToolkitToolResult: { mock: { data: () => ({ ok: true, result: {} }) } },
        },
        query: { useQuery: true, useSuspenseQuery: false, signal: true },
        zod: {
          generate: { param: true, query: true, header: true, body: true, response: true },
          generateReusableSchemas: true,
        },
      },
    },
    hooks: {
      afterAllFilesWrite: [
        () =>
          backfillMissingZodModels({
            modelDir: path.join(OUT_DIR, 'model'),
            generatedDir: OUT_DIR,
          }),
        'prettier --write',
      ],
    },
  },
});
