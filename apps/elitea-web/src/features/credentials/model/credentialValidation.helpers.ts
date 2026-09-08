/**
 * `useCredentialValidation`'s pure helpers, split out of
 * `./useCredentialValidation.ts` so that hook file stays under the §3.5
 * 400-line budget (it was at 392 when the refusal REASON had to be carried
 * as well as the message — see `applyStoredRow` below). Nothing here holds
 * state; every function is the same one the hook already called, moved
 * verbatim except for the `setReasons` writer `applyStoredRow` now takes.
 *
 * Same "helpers beside the hook" shape `features/toolkits`'
 * `model/credentialWarning.helpers.ts` already uses next to its own
 * `useCredentialWarning.hooks.ts`.
 */
import type { CredentialValidationStatus } from './credentialValidationStatus';

/** One stored-check result row, in the two forms the single and batch routes answer with. */
interface StoredCheckRowLike {
  readonly success?: boolean | undefined;
  readonly message?: string | undefined;
  readonly unsupported?: boolean | undefined;
  /**
   * The toolkit probe's closed vocabulary — `ok`, `auth_failed`, `unreachable`,
   * `unsupported_type` (`internal/api/v2/configurations/toolkit_check.go`).
   * Absent on the LLM path, which collapses its own reasons into `message`.
   */
  readonly reason?: string | undefined;
}

/**
 * Turns one stored-check row into a status and, when it failed, a message.
 *
 * A type this build cannot probe is `unsupported` and NOT `invalid`: the
 * attention indicator means "this credential is broken", and a missing check is
 * not evidence of that. Both the `unsupported` flag (a type the catalogue never
 * heard of) and `reason: 'unsupported_type'` (a known type with no probe) land
 * there.
 */
export function applyStoredRow(
  credentialId: string,
  row: StoredCheckRowLike,
  setStatuses: (updater: (prev: Record<string, CredentialValidationStatus>) => Record<string, CredentialValidationStatus>) => void,
  setMessages: (updater: (prev: Record<string, string>) => Record<string, string>) => void,
  setReasons: (updater: (prev: Record<string, string>) => Record<string, string>) => void,
): void {
  if (row.unsupported === true || row.reason === 'unsupported_type') {
    setStatuses((prev) => ({ ...prev, [credentialId]: 'unsupported' }));
    return;
  }
  const isValid = row.success === true;
  setStatuses((prev) => ({ ...prev, [credentialId]: isValid ? 'valid' : 'invalid' }));
  if (!isValid && row.message !== undefined && row.message !== '') {
    setMessages((prev) => ({ ...prev, [credentialId]: row.message ?? '' }));
  }
  // The REASON, kept apart from the message. A refusal that carries one is a
  // verdict about the credential; a refusal that carries none is this
  // deployment saying it could not ask. Callers that gate an action on the
  // check (the toolkit editor's Save) must be able to tell those apart, and
  // the message text cannot — see `credentialValidationStatus.ts`.
  if (!isValid && row.reason !== undefined && row.reason !== '') {
    setReasons((prev) => ({ ...prev, [credentialId]: row.reason ?? '' }));
  }
}

/** `EliteaApiError.failure.kind === 'http'` carries the numeric status; anything else (network/auth/aborted) has none. */
export function getHttpStatus(error: unknown): number | undefined {
  if (typeof error !== 'object' || error === null || !('failure' in error)) return undefined;
  const failure = (error as { failure?: unknown }).failure;
  if (typeof failure !== 'object' || failure === null) return undefined;
  const record = failure as { kind?: unknown; status?: unknown };
  if (record.kind !== 'http') return undefined;
  return typeof record.status === 'number' ? record.status : undefined;
}

/**
 * Best-effort message extraction for a THROWN test-connection failure
 * (adversarial-review finding: this catch branch previously discarded the
 * failure entirely, so `getCredentialMessage()` silently returned `''` for
 * the most common real-world validation-failure path — a non-2xx response,
 * as opposed to the 2xx-with-`{error}`-body case the try branch above
 * already handles via `result.error`).
 *
 * `error.failure.body` is `HttpFailure`'s parsed-JSON-or-raw-text response
 * body (`shared/api/http.ts`'s `toResult`) for an `'http'`-kind failure —
 * duck-typed locally, same shape/convention as this file's own
 * `getHttpStatus` above, rather than importing `EliteaApiError` from
 * `@/shared/api/generated/mutator`, to keep one duck-typing style per file.
 * The `body.error ?? body.message` precedence mirrors the two other
 * call sites in this codebase that already extract a message from this
 * exact failure shape: `pages/credentials/useCredentialFormController.ts`'s
 * `toCredentialApiError` (`record['error'] ?? record['message']`) and
 * `features/mcps/lib/registerDynamicClient.ts`'s `extractOAuthErrorDetail`.
 * Returns `undefined` (never a synthesized generic string) when the body
 * carries no such text, so a real "no message available" case still
 * degrades to `getCredentialMessage()`'s existing `''` fallback instead of
 * inventing wording the server never sent.
 */
export function getHttpErrorMessage(error: unknown): string | undefined {
  return extractMessageFromFailureBody(getHttpFailureBody(error));
}

/** Pulls the `'http'`-kind `HttpFailure`'s raw `body` out of a thrown error — same duck-typing as `getHttpStatus` above, split out purely so `getHttpErrorMessage` stays within the §3.5 cyclomatic-complexity budget. `undefined` for anything else (network/auth/aborted failures, or a non-`EliteaApiError` throw). */
export function getHttpFailureBody(error: unknown): unknown {
  if (typeof error !== 'object' || error === null || !('failure' in error)) return undefined;
  const failure = (error as { failure?: unknown }).failure;
  if (typeof failure !== 'object' || failure === null) return undefined;
  const record = failure as { kind?: unknown; body?: unknown };
  if (record.kind !== 'http') return undefined;
  return record.body;
}

/** `body.error ?? body.message` (or the body itself when it's a raw string) — see `getHttpErrorMessage`'s doc comment above for the convention this mirrors. Non-empty strings only. */
function extractMessageFromFailureBody(body: unknown): string | undefined {
  if (typeof body === 'string') return body !== '' ? body : undefined;
  if (typeof body !== 'object' || body === null) return undefined;
  const bodyRecord = body as Record<string, unknown>;
  const message = bodyRecord['error'] ?? bodyRecord['message'];
  return typeof message === 'string' && message !== '' ? message : undefined;
}
