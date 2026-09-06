import { EliteaApiError } from '@/shared/api/generated/mutator';

/**
 * Renders a TanStack Query `error` (an `EliteaApiError`, per
 * `shared/api/generated/mutator.ts` — never the RTK-Query-shaped
 * `{status, data}` object the baseline's `common/utils.js` `buildErrorMessage`
 * expects) into a display string.
 *
 * Not a port of the baseline's `buildErrorMessage` (used throughout
 * `hooks/application/*` — `useSaveVersion.js`, `useSaveNewVersion.js`,
 * `useDeleteVersion.js`, `useSaveChangedTools.js`,
 * `useValidateApplicationVersion.js`, `useApplicationChatSwitchVersion.js`):
 * that function's whole dispatch table (`err.status === 403`,
 * `err.data.message`, a FastAPI/Pydantic validation-error array, ...) is
 * shaped for the OLD RTK-Query transport's error object AND the old
 * pylon/FastAPI backend's per-field validation envelope. The real Go
 * backend's documented error convention (`shared/api/generated/model/
 * applicationRelationList.zod.ts`'s own module doc: "General errors (4xx/5xx):
 * `{"error": "message"}`") carries a flat message, not a `[{loc, msg, ctx}]`
 * array — grepped the whole generated client for any per-field validation
 * envelope on these operations, zero hits. `EliteaApiError` already carries a
 * real, readable `.message` (`mutator.ts`'s `describeFailure`), so no dispatch
 * table is needed. Same fallback shape as `features/apps/lib/errorMessage.ts`
 * (`appDetailErrorMessage`, the first Wave-2 unit to hit this exact
 * situation) and `src/routes/-ui/RouteStatus.tsx`'s `RouteError` — not
 * imported (`no-sideways-features`/R-L3 forbid reaching into another
 * feature's internals), rebuilt locally, three bytes of logic.
 */
export function applicationErrorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/**
 * `applicationErrorMessage(error) || fallback` — but honest about WHEN a
 * shape is genuinely parseable, unlike `applicationErrorMessage` alone.
 *
 * **Confirmed regression fix (A1-application-chat cluster, finding 3):**
 * `useApplicationChatStreaming.hooks.ts`'s `onDeleteMessage`/
 * `onDeleteAllMessages` port `useApplicationChat.hooks.js:627,669`'s
 * `buildErrorMessage(result.error) || 'Failed to delete the message, please
 * try again.'` — a pattern that only works if the left side can actually
 * come back falsy for a shape it can't meaningfully describe. Baseline's
 * `buildErrorMessage` (the old RTK-Query/pydantic-envelope parser) does that
 * for an unrecognised shape; `applicationErrorMessage` does NOT — for any
 * non-`Error`, non-`string` rejection (e.g. a plain object) it falls through
 * to `String(error)`, which degrades to the literal `"[object Object]"`
 * (always truthy), so the `||` never reaches the friendly fallback and the
 * user sees that literal string. This helper restores the baseline's actual
 * fallback behaviour without changing `applicationErrorMessage`'s own
 * documented, tested, "stringify anything" contract (used elsewhere for
 * genuinely-always-real-Error `EliteaApiError` rejections): only an `Error`
 * instance or a `string` counts as "parseable" here; anything else means
 * `fallback`.
 */
export function applicationErrorMessageOrFallback(error: unknown, fallback: string): string {
  return error instanceof Error || typeof error === 'string' ? applicationErrorMessage(error) || fallback : fallback;
}

/**
 * The SERVER's own explanation, when it sent one.
 *
 * #147. `applicationErrorMessage` above returns `EliteaApiError.message`,
 * which `mutator.ts`'s `describeFailure` builds as
 * `eliteaFetch: 400 from <url>`. That string is a diagnostic, not an
 * explanation, and putting it in front of a user is the same as saying
 * nothing. The application endpoints answer a refusal with a flat
 * `{"error": "message"}` body (the convention `applicationErrorMessage`'s own
 * doc comment cites), and `HttpFailure.body` holds it parsed. The delete
 * endpoint's refusal — "Unpublish first. Cannot delete a published
 * version." — reaches the user only through this function.
 *
 * The same extraction `features/settings/api/ai-configuration/api.ts`'s
 * `modelConfigurationErrorMessage` already does for the model routes.
 * Rebuilt here rather than imported: `no-sideways-features`/R-L3 forbid
 * reaching into another feature's internals.
 *
 * `applicationErrorMessage` keeps its contract unchanged; every existing
 * caller still gets exactly what it got.
 */
export function applicationServerErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof EliteaApiError) {
    /* `fallback`, never `error.message`: for an `EliteaApiError` that message
       is always the `eliteaFetch: <status> from <url>` diagnostic. A refusal
       the server did not explain is better reported in the app's own words. */
    return error.failure.kind === 'http' ? (serverBodyMessage(error.failure.body) ?? fallback) : fallback;
  }
  return applicationErrorMessageOrFallback(error, fallback);
}

/** The flat `{"error": "…"}` (or `{"message": "…"}`) body the Go handlers answer a refusal with. `undefined` when the body says nothing readable. */
function serverBodyMessage(body: unknown): string | undefined {
  if (typeof body === 'string' && body !== '') return body;
  if (typeof body !== 'object' || body === null) return undefined;
  const record = body as Record<string, unknown>;
  const detail = record['error'] ?? record['message'];
  return typeof detail === 'string' && detail !== '' ? detail : undefined;
}
