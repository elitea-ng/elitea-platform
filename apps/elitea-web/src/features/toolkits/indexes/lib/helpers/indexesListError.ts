/**
 * What the rail says when the index list could not be READ.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * THE DEFECT THIS CLOSES
 * ─────────────────────────────────────────────────────────────────────────
 * `IndexesList` had one non-loading branch: `!indexesList.length && !loading`
 * → "Still no indexes created". A failed query produces exactly that state —
 * `useIndexesListQuery` leaves `data` undefined, `IndexesContainer` falls back
 * to `EMPTY_INDEX_LIST`, and the rail then reports, in the product's own
 * voice, a fact about the toolkit that it does not know.
 *
 * That is not hypothetical. The 2026-09-06 production-parity walk (row F4 of
 * the validation matrix) recorded exactly one HTTP failure across the whole
 * local session:
 *
 *   GET /api/v2/elitea_core/index_meta/prompt_lib/2/1
 *   → 400 {"error":"PGVector configuration is missing for toolkit 1"}
 *
 * and the screen it produced said "Still no indexes created". The server had
 * named the missing prerequisite precisely, in a sentence written for a human,
 * and the client threw it away. The whole feature reads as empty-but-fine when
 * it is in fact unusable until a vector store is configured — and there is no
 * hint anywhere on the screen that the two facts are related.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * WHY THE SERVER'S OWN SENTENCE, AND NOT A TRANSLATED ONE
 * ─────────────────────────────────────────────────────────────────────────
 * `writeCurrentIndexMetaError`
 * (`services/elitea-main/internal/api/v2/indexing/index_meta.go:126-142`)
 * writes five distinct sentences, three of which name the toolkit id. They
 * are the diagnosis; restating them as one generic "could not load indexes"
 * would keep the failure visible and still throw away everything that makes
 * it actionable. So the body's `error` string is shown verbatim, under a
 * translated heading that supplies the context the sentence itself lacks.
 *
 * The generic heading is the fallback for every failure with no readable
 * body — a network drop, a proxy error page, a 502 with an HTML body. Those
 * must still be reported as failures rather than as an empty list, which is
 * the whole point.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * PREREQUISITE GUIDANCE
 * ─────────────────────────────────────────────────────────────────────────
 * `isMissingVectorStoreError` recognises the one failure the user can fix
 * themselves without leaving the product, so the rail can point at the page
 * that fixes it. The match is on the SERVER's wording, which is a coupling
 * and is declared as one: it is matched loosely (case-insensitive, on the two
 * stable words) so a rewording that keeps the subject still matches, and a
 * miss degrades to the plain message rather than to silence.
 */

/** `HttpFailure`'s `kind: 'http'` arm, structurally — `shared/api/http.ts:45-49`. */
interface HttpFailureLike {
  readonly kind?: unknown;
  readonly status?: unknown;
  readonly body?: unknown;
}

/** `EliteaApiError`, structurally — `shared/api/generated/mutator.ts:110`. Matched by shape so this helper stays a pure module with no api import. */
interface ApiErrorLike {
  readonly failure?: HttpFailureLike;
}

function readFailure(error: unknown): HttpFailureLike | undefined {
  if (typeof error !== 'object' || error === null) return undefined;
  const failure = (error as ApiErrorLike).failure;
  if (typeof failure !== 'object' || failure === null) return undefined;
  return failure;
}

/**
 * The server's own sentence, or `undefined` when the failure carries none.
 *
 * elitea-main's `writeError` puts it at `body.error`; `body.message` and
 * `body.detail` are the two other shapes this API family uses (the generated
 * error envelope and FastAPI-shaped validation errors respectively), read here
 * so a failure from either does not fall through to the generic heading.
 */
export function readIndexesListErrorMessage(error: unknown): string | undefined {
  const failure = readFailure(error);
  if (failure === undefined || failure.kind !== 'http') return undefined;
  const body = failure.body;
  if (typeof body === 'string') return body.trim() === '' ? undefined : body.trim();
  if (typeof body !== 'object' || body === null) return undefined;
  const record = body as Record<string, unknown>;
  for (const key of ['error', 'message', 'detail'] as const) {
    const value = record[key];
    if (typeof value === 'string' && value.trim() !== '') return value.trim();
  }
  return undefined;
}

/** The HTTP status of a failed read, when there is one. Rendered beside the message so a 502 is not mistaken for a rejection. */
export function readIndexesListErrorStatus(error: unknown): number | undefined {
  const failure = readFailure(error);
  if (failure === undefined || failure.kind !== 'http') return undefined;
  return typeof failure.status === 'number' ? failure.status : undefined;
}

/**
 * True for the "this project has no vector store" failure — the one the user
 * can fix, and the one the parity walk actually hit.
 *
 * `index_meta.go:135` writes "PGVector configuration is missing for toolkit N";
 * `index_meta_delete.go:163` writes "Connection string is missing in PGVector
 * configuration for toolkit N" for the half-configured case. Both are the same
 * problem to the person reading the screen, so both match.
 */
export function isMissingVectorStoreError(message: string | undefined): boolean {
  if (message === undefined) return false;
  const normalized = message.toLowerCase();
  return normalized.includes('pgvector') && normalized.includes('missing');
}
