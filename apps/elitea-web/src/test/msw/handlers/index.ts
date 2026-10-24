import type { RequestHandler } from 'msw';

import { getAdminMock } from '../../../shared/api/generated/admin/admin.msw';
import { getAnalyticsMock } from '../../../shared/api/generated/analytics/analytics.msw';
import { getApplicationsMock } from '../../../shared/api/generated/applications/applications.msw';
import { getArtifactsMock } from '../../../shared/api/generated/artifacts/artifacts.msw';
import { getAuthMock } from '../../../shared/api/generated/auth/auth.msw';
import { getDefaultMock } from '../../../shared/api/generated/default/default.msw';
import { getSecretsMock } from '../../../shared/api/generated/secrets/secrets.msw';
import { getSettingsMock } from '../../../shared/api/generated/settings/settings.msw';
import { getSkillsMock } from '../../../shared/api/generated/skills/skills.msw';
import { getTagsMock } from '../../../shared/api/generated/tags/tags.msw';
import { getToolkitsMock } from '../../../shared/api/generated/toolkits/toolkits.msw';

/**
 * MSW handler registry (spec §6.5 R-M2/R-M3), unit M1.
 *
 * ── What's in `handlers` and why ──────────────────────────────────────────
 *
 * `handlers` is the DEFAULT registry `src/test/setup.ts` boots via
 * `setupServer(...handlers)`. It is populated ENTIRELY from orval's
 * generated `*.msw.ts` mock skeletons (`src/shared/api/generated/<tag>/
 * <tag>.msw.ts`, 11 tags — the `chat` tag went away with #126, which deleted
 * the two operations it held (`getChatConfig`, `webchatSync`) along with the
 * prototype-transport routes behind them — see orval.config.ts's
 * `output.mock` and endpoints.manifest.json; the operation count is not
 * restated here because it drifts every time the spec grows, as it did
 * when #151 added the `secrets` tag). Every generated tag exports a
 * `get<Tag>Mock()` aggregate (e.g. `getAuthMock`); spreading all 12 covers
 * every endpoint the manifest knows about with a self-consistent, always-
 * in-sync-with-the-OpenAPI-spec default response.
 *
 * ── Why the 4 hand-authored files (transport/upload/artifacts/download)
 *    are DELIBERATELY NOT spread in here ─────────────────────────────────
 *
 * `handlers/{transport,upload,artifacts,download}.ts` (units F4/S6) do not
 * export a static `RequestHandler[]` at all — every export is a FACTORY
 * function (`probeOk()`, `chunkAckInProgress(sink?)`, `objectUploadOk(sink?)`,
 * `exportOk(filename, sink?)`, …), several parameterised by mutable
 * `SessionGate`s, call-count sequences, or request-capture sinks that only
 * make sense scoped to ONE test. Their own module docs say so explicitly
 * (transport.ts: "Tests attach these per-scenario via `server.use(...)`;
 * nothing registers globally … stateful gates must not leak between
 * tests"), and every consumer (src/shared/api/{artifacts,upload}.test.ts,
 * src/shared/lib/download.test.ts, src/routes/auth-callback.test.tsx,
 * src/shared/api/{http,http.reauth,auth/verify-session}.test.ts, …) already
 * calls them via `server.use(...)` inside individual `it()`/`beforeEach`
 * blocks, never through this file. Spreading a *called* instance of these
 * factories into the base registry would (a) not typecheck for the
 * parameterised ones without inventing arbitrary defaults, and (b) reinstate
 * exactly the cross-test state leakage `afterEach(() => server.resetHandlers())`
 * in src/test/setup.ts exists to prevent. So there is no "route-pattern
 * overlap" to resolve by precedence at the array level: the two sources
 * never occupy the same array. Where a hand-authored factory's route DOES
 * overlap a generated one (e.g. `download.ts`'s `exportOk` and generated
 * `applications.msw.ts`'s `getExportApplicationMockHandler` both match GET
 * `*(/)elitea_core/export_import/prompt_lib/*` (msw wildcard-prefix pattern,
 * written with a parenthesised slash here only so it doesn't close this
 * comment block) — the real backend endpoint is
 * content-negotiated by `?format=md` query string, which MSW does not
 * path-match on), the per-test `server.use()` call always wins: msw
 * prepends runtime handlers ahead of the base list, so the hand-authored,
 * real-shape response is what that test actually receives regardless of
 * what this file exports. This file only has to be internally
 * non-contradictory with ITSELF, and it is (12 domains, no duplicate tags).
 *
 * ── R-M3 (validate every handler response against its zod schema AT
 *    REGISTRATION TIME) for generated vs. hand-authored handlers ─────────
 *
 * R-M3 is enforced two different, deliberately different ways, and both are
 * "at registration time" for what registration means in that source:
 *
 *  - Hand-authored (transport/upload/artifacts/download.ts): every exported
 *    factory routes its handler through `registerValidatedHandlers` (see
 *    each file's local `validated()` helper) at the moment the factory is
 *    CALLED — which for these is exactly when a test registers it via
 *    `server.use(probeOk())` etc. The fixture is an untyped, hand-maintained
 *    JSON file (`../fixtures/**\/*.json`) that COULD drift from its zod
 *    schema with no compiler to catch it — that's the entire reason R-M3's
 *    runtime `safeParse` exists, and it fires every time.
 *
 *  - Generated (`*.msw.ts`, spread into `handlers` below): these are NOT
 *    routed through `registerValidatedHandlers`, and that is a decision,
 *    not an oversight. Every generated mock body comes from a
 *    `get<Op>ResponseMock()` faker generator whose return type is the exact
 *    TS type orval derived from the SAME `override.zod` schema pass that
 *    produces the operation's response validator (orval.config.ts; see
 *    `src/shared/api/generated/model/*.zod.ts`) — schema and mock are two
 *    outputs of one codegen run against the same `v2.yaml`, so a
 *    mock-vs-schema mismatch is a `tsc --noEmit` failure, not a runtime
 *    condition, and `npm run typecheck` already gates every PR (§6.6). A
 *    `zod.safeParse` at registration would at best re-prove something the
 *    type system already guarantees and at worst pass silently on an `any`
 *    escape hatch the type check would have caught. More importantly,
 *    `registerValidatedHandlers` also asserts the `fixture.recordedAt`
 *    precondition — generated handlers have no `fixture` file and no
 *    Channel-B recording at all (`useExamples: false` in orval.config.ts's
 *    mock output; they are synthetic Channel-A data by construction), so
 *    the only way to route them through that function would be to invent a
 *    fabricated `recordedAt` timestamp with no real recording behind it —
 *    exactly the "fake fixture with a fabricated recordedAt" this unit was
 *    told not to do.
 *
 * R-M2 (every handler lives in `src/test/msw/handlers/` and either derives
 * from a fixture or is orval-generated) is satisfied for this file's own
 * contents by construction (nothing here is a handler body, only imports of
 * already-compliant sources), and `scripts/check-handlers.mjs` scans this
 * directory's hand-authored `*.ts` files for the fixture-derivation rule
 * (R-M2's other clause) while intentionally NOT walking
 * `src/shared/api/generated/**` — that tree satisfies R-M2 via its OWN
 * clause ("generated by orval") and is `Do not edit manually` codegen
 * output the check-handlers heuristic does not own the shape of; running it
 * there would either be a no-op (the generated bodies are never object/array
 * literals — see `getXResponseMock()` calls, always a variable/conditional
 * argument, never an inline literal) or a false positive against a template
 * shape a future orval upgrade could change out from under this repo.
 */
export const handlers: RequestHandler[] = [
  ...getAdminMock(),
  ...getAnalyticsMock(),
  ...getApplicationsMock(),
  ...getArtifactsMock(),
  ...getAuthMock(),
  ...getDefaultMock(),
  ...getSecretsMock(),
  ...getSettingsMock(),
  ...getSkillsMock(),
  ...getTagsMock(),
  ...getToolkitsMock(),
];
