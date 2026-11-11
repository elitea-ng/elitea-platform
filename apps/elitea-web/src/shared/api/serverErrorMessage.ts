/**
 * The server's own sentence for a refused call (issue 1848), read ONE way.
 *
 * `EliteaApiError.message` is the `eliteaFetch: <status> from <url>`
 * diagnostic `mutator.ts`'s `describeFailure` builds: a URL is not an
 * explanation, and putting it in front of a person says nothing. The Go
 * handlers answer a refusal with the flat `{"error": "…"}` body
 * (`pkg/apierr`), a few older routes with `{"message": "…"}` or a plain
 * string, and `HttpFailure.body` holds it parsed.
 *
 * Every feature used to carry its own copy of this reader (agents, agent
 * teams, webhooks, memories, proposals, agent memory, evaluation, model
 * configuration); the copies differed (some read `message` or a string body,
 * some did not), so the same refusal read differently across the app.
 */
import { EliteaApiError } from './generated/mutator';

/**
 * The sentence a refusal body carries: a plain-text body (a Go `http.Error`
 * line, trimmed), else the object's non-empty `error`, else its non-empty
 * `message`. `undefined` when the body says nothing readable — an empty body,
 * or markup (a proxy's HTML error page is not a sentence for a person).
 */
export function serverErrorSentence(body: unknown): string | undefined {
  if (typeof body === 'string') {
    const text = body.trim();
    return text === '' || text.startsWith('<') ? undefined : text;
  }
  if (typeof body !== 'object' || body === null) return undefined;
  const record = body as Record<string, unknown>;
  for (const key of ['error', 'message']) {
    const detail = record[key];
    if (typeof detail === 'string' && detail !== '') return detail;
  }
  return undefined;
}

/** The server's sentence for a failed call: an HTTP refusal whose body carries one. `undefined` for anything else (network, abort, a thrown `Error`). */
export function serverErrorOf(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') return undefined;
  return serverErrorSentence(error.failure.body);
}

/** The server's sentence for a failed call, else `fallback` (never the `eliteaFetch` diagnostic). */
export function serverErrorMessage(error: unknown, fallback: string): string {
  return serverErrorOf(error) ?? fallback;
}
