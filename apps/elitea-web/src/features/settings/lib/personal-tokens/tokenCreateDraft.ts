/**
 * The token form across a fresh sign-in (ADR-0059 R1.3b).
 *
 * Giving a token administration scopes needs a sign-in of the last few
 * minutes. When the create answers 401 `reauthentication_required`, the page
 * saves what the person typed, leaves for the sign-in the server named, and
 * comes back to the same page, which restores the form ONCE. The secret of a
 * token is never part of the draft: nothing was created.
 */
import {
  reauthenticationUrl,
  recentSignInRequest,
  saveReauthenticationDraft,
  takeReauthenticationDraft,
} from '@/shared/api/auth/reauthentication';

export interface TokenCreateDraft {
  readonly name: string;
  readonly measure: string;
  readonly expiration: number | undefined;
  readonly adminScopes: readonly string[];
}

const DRAFT_KEY = 'token-create';

function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((item) => typeof item === 'string');
}

/** The saved form, read once, or `null` when there is none or it is malformed. */
export function takeTokenCreateDraft(): TokenCreateDraft | null {
  const raw = takeReauthenticationDraft(DRAFT_KEY) as Partial<Record<keyof TokenCreateDraft, unknown>> | undefined;
  if (raw === undefined || raw === null || typeof raw !== 'object') return null;
  if (typeof raw.name !== 'string' || typeof raw.measure !== 'string' || !isStringArray(raw.adminScopes)) return null;
  return {
    name: raw.name,
    measure: raw.measure,
    expiration: typeof raw.expiration === 'number' ? raw.expiration : undefined,
    adminScopes: raw.adminScopes,
  };
}

/** The form's starting values: the restored draft, or the page's own defaults. */
export function tokenFormDefaults(
  draft: TokenCreateDraft | null,
  defaultExpiration: number,
): { name: string; measure: string; expiration: number | undefined } {
  if (draft === null) return { name: '', measure: 'days', expiration: defaultExpiration };
  return { name: draft.name, measure: draft.measure, expiration: draft.expiration };
}

/** Navigation seam, so tests do not navigate jsdom. */
export interface SignInNavigator {
  readonly assign: (url: string) => void;
  readonly currentPath: () => string;
}

const browserNavigator: SignInNavigator = {
  assign: (url) => window.location.assign(url),
  currentPath: () => window.location.pathname + window.location.search,
};

/**
 * What a create failure means for the sign-in round trip:
 * - `none`: it is not a recent-sign-in 401; the page shows its usual message.
 * - `started`: the draft is saved and the browser is on its way to the sign-in.
 * - `unavailable`: a recent sign-in is needed, but this session has no
 *   browser sign-in to renew it; the page says so.
 */
export type SignInOutcome = 'none' | 'started' | 'unavailable';

export function signInAgainForTokenCreate(
  error: unknown,
  draft: TokenCreateDraft,
  navigator: SignInNavigator = browserNavigator,
): SignInOutcome {
  const request = recentSignInRequest(error);
  if (request === null) return 'none';
  const url = reauthenticationUrl(request, navigator.currentPath());
  if (url === null) return 'unavailable';
  saveReauthenticationDraft(DRAFT_KEY, draft);
  navigator.assign(url);
  return 'started';
}
