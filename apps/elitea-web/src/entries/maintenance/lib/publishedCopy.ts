/**
 * The operator's splash copy, read from the platform at run time.
 *
 * ## Why this entry fetches anything at all
 *
 * It used to take everything from BUILD-TIME `VITE_MAINTENANCE_*` variables,
 * for a stated reason: an ingress serves this page when the platform is fully
 * down, elitea-main included, so it "cannot ask the API anything". That is true
 * of the case it was written for and false as a general rule — the same page is
 * also served during a PLANNED window, when elitea-main is up and answering
 * `platform_settings`, which is where the operator's title and splash body
 * actually live.
 *
 * So it asks, and it treats not getting an answer as normal rather than as an
 * error. A build-time variable cannot be changed without a redeploy, which is
 * the one thing an operator cannot do in the middle of a window.
 *
 * ## Why plain fetch and not the generated client
 *
 * This entry has no `AppProviders` and no react-query, deliberately — it is
 * built with `viteSingleFile` into one document. Pulling the client in would
 * bring the query cache, the re-auth flow and the whole generated surface into
 * a page whose job is to render one paragraph offline.
 *
 * ## The rules, and why each one
 *
 *  - A failure of ANY kind resolves to "nothing published". The page then shows
 *    exactly what it showed before this module existed. There is no error state
 *    to render: a splash that says it could not load the splash is worse than
 *    the splash.
 *  - The request is aborted after a short timeout. This page is often loaded
 *    against a platform that is not answering, and a fetch that hangs would
 *    leave the copy in limbo for as long as the browser allows.
 *  - Nothing here sanitises. The renderer does, immediately before it injects.
 */

/** The endpoint the app shell already polls for the same object. */
const SETTINGS_URL = '/api/v2/elitea_core/platform_settings/prompt_lib';

/** Long enough for a slow answer, short enough not to hold a dead platform. */
const TIMEOUT_MS = 3_000;

export interface PublishedMaintenanceCopy {
  /** Heading the operator authored, or '' to use this page's own. */
  readonly title: string;
  /** HTML body the operator authored, or '' to use this page's own content. */
  readonly html: string;
  /** Markdown-ish message the operator authored. Rendered as TEXT here. */
  readonly message: string;
}

export const NO_PUBLISHED_COPY: PublishedMaintenanceCopy = { title: '', html: '', message: '' };

function textOf(raw: unknown): string {
  return typeof raw === 'string' ? raw.trim() : '';
}

/** Narrow the settings document down to the maintenance copy, or nothing. */
export function readPublishedCopy(payload: unknown): PublishedMaintenanceCopy {
  if (typeof payload !== 'object' || payload === null) return NO_PUBLISHED_COPY;
  const maintenance = (payload as { maintenance?: unknown }).maintenance;
  if (typeof maintenance !== 'object' || maintenance === null) return NO_PUBLISHED_COPY;
  const row = maintenance as Record<string, unknown>;
  return {
    title: textOf(row.title),
    html: textOf(row.html),
    message: textOf(row.message),
  };
}

/** Ask the platform for the operator's copy. Never rejects. */
export async function fetchPublishedCopy(
  fetchImpl: typeof fetch = fetch,
): Promise<PublishedMaintenanceCopy> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS);
  try {
    const response = await fetchImpl(SETTINGS_URL, {
      signal: controller.signal,
      credentials: 'same-origin',
      headers: { Accept: 'application/json' },
    });
    if (!response.ok) return NO_PUBLISHED_COPY;
    return readPublishedCopy(await response.json());
  } catch {
    // Every failure is the same failure here: the platform did not answer, so
    // this page shows its own words. See the module doc.
    return NO_PUBLISHED_COPY;
  } finally {
    clearTimeout(timer);
  }
}
