/**
 * The re-sign-in helper (ADR-0059 R1.3b): what a page does with a
 * `reauthentication_required` 401.
 *
 * The server asks for a sign-in younger than `max_age` seconds and names the
 * sign-in that renews this session (`X-Elitea-Reauthentication-Method`).
 * `shared/api/reauth-policy.ts` folds both headers into the failure body, and
 * this module turns them into the login start of `admin/authentication`
 * "Starting a sign-in with a limit":
 *
 *   OIDC  /auth/oidc/login?max_age=<limit>&target_to=<path>
 *   SAML  /auth/saml/login?max_age=<limit>&target_to=<path>
 *   Form  /auth/form/reauthenticate?target_to=<path>
 *
 * Reusable by any action behind the recent-sign-in check (break-glass and the
 * critical actions come next). The page keeps its own draft across the
 * navigation: see `saveReauthenticationDraft`.
 */
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { createStorage } from '@/shared/lib/storage';

import { TARGET_TO_PARAM } from './constants';

type ReauthenticationMethod = 'oidc' | 'saml' | 'form';

export interface RecentSignInRequest {
  /** The limit in whole seconds, or `null` when the server sent none. */
  readonly maxAge: number | null;
  /** The sign-in that renews this session, or `null` when it has none. */
  readonly method: ReauthenticationMethod | null;
}

const METHODS: readonly ReauthenticationMethod[] = ['oidc', 'saml', 'form'];

/** The request a failure carries, or `null` when it is not a recent-sign-in 401. */
export function recentSignInRequest(error: unknown): RecentSignInRequest | null {
  if (!(error instanceof EliteaApiError)) return null;
  const { failure } = error;
  if (failure.kind !== 'http' || failure.status !== 401) return null;
  const asked = (failure.body as { readonly reauthentication?: { readonly max_age?: unknown; readonly method?: unknown } } | null)
    ?.reauthentication;
  if (asked === undefined || asked === null) return null;
  const method = METHODS.find((known) => known === asked.method) ?? null;
  return { maxAge: typeof asked.max_age === 'number' ? asked.max_age : null, method };
}

/**
 * The login start that renews the session, returning to `targetTo`, or `null`
 * when the session has no browser sign-in to renew it (the page then says so).
 */
export function reauthenticationUrl(request: RecentSignInRequest, targetTo: string): string | null {
  const target = targetTo.startsWith('/') ? targetTo : `/${targetTo}`;
  const params = new URLSearchParams();
  if (request.method === null) return null;
  if (request.method === 'form') {
    params.set(TARGET_TO_PARAM, target);
    return `/auth/form/reauthenticate?${params.toString()}`;
  }
  params.set('max_age', String(request.maxAge ?? 0));
  params.set(TARGET_TO_PARAM, target);
  return `/auth/${request.method}/login?${params.toString()}`;
}

/* ── the draft that survives the sign-in ─────────────────────────────────── */

/** Logical key: `createStorage` adds the `el.` namespace, so logout sweeps it. */
const DRAFT_PREFIX = 'reauth.draft.';

/**
 * Keeps a form's values across the sign-in round trip. sessionStorage is per
 * tab and survives the navigation; a draft is read ONCE and removed, so a
 * later visit starts empty. Storage that throws (private mode, blocked site
 * data) loses the draft and nothing else.
 */
export function saveReauthenticationDraft(key: string, draft: unknown): void {
  try {
    createStorage('session').setJSON(DRAFT_PREFIX + key, draft);
  } catch {
    // Handled (§3.6): the sign-in still works; the person types again.
  }
}

export function takeReauthenticationDraft(key: string): unknown {
  try {
    const session = createStorage('session');
    const draft = session.getJSON<unknown>(DRAFT_PREFIX + key);
    session.remove(DRAFT_PREFIX + key);
    return draft ?? undefined;
  } catch {
    // Handled (§3.6): an unreadable draft is no draft.
    return undefined;
  }
}
