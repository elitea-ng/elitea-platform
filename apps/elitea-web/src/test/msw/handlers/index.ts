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

import { exportDocumentDefault } from './download';

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
 * Where an operation carries a response `example` in v2.yaml, orval's
 * `useExamples: true` makes that example the generated body, so the default
 * response is the contract's own documented one rather than faker noise.
 *
 * ── Why the 4 hand-authored files (transport/upload/artifacts/download)
 *    are NOT spread in here (one stateless exception) ─────────────────────
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
 * in src/test/setup.ts exists to prevent. The one exception is
 * `exportDocumentDefault()` (download.ts): stateless, and listed FIRST so it
 * shadows the generated exportApplication handler, which answers an empty
 * octet-stream because the operation also declares an application/zip body.
 * Where a hand-authored factory's route DOES
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
 * ── R-M2/R-M3 for generated vs. hand-authored handlers ──────────────────
 *
 *  - Generated (`*.msw.ts`, spread into `handlers` below): every body comes
 *    from a `get<Op>ResponseMock()` whose return type is the TS type orval
 *    derived from v2.yaml, so a mock-vs-schema mismatch is a `tsc` failure.
 *    Where the body is the operation's `example`,
 *    `scripts/check-spec-examples.mjs` checks it against its schema (R-M3).
 *  - Hand-authored (transport/upload/artifacts/download.ts): each body is
 *    either one of those generated mocks or a typed module constant (typed
 *    with the generated model type where the contract has one).
 *    `scripts/check-handlers.mjs` (R-M2) walks this directory and fails on an
 *    inline literal body, an untyped constant or a JSON import; it does not
 *    walk `src/shared/api/generated/**`, which is codegen output.
 */
export const handlers: RequestHandler[] = [
  // Ahead of the generated applications handlers: the generated one for
  // exportApplication answers an empty octet-stream (see download.ts).
  exportDocumentDefault(),
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
