import { createApiClient } from '@/api/client';
import { isApiError } from '@/api/errors';
import type { BaseFetch } from '@/api/middleware';
import { fetchProjects, resolveProjectSelection } from '@/api/queries/projects';
import { log } from '@/lib/log';
import { SECURE_KEYS, boundWorkspace, deleteSecureItemIn, setSecureItemIn } from '@/security/secureStorage';
import { activateSignedInWorkspace } from '@/security/workspaceRegistry';
import { getSession, setCredential, setSession } from '@/state/session';

/**
 * Developer sign-in: paste a personal access token.
 *
 * A PAT is a full user credential with optional or no expiry, so this path
 * exists for development only: it is enabled exactly when `__DEV__` is true.
 * Release builds (including internal preview builds) sign in with the
 * workspace's native sign-in, and no configuration can turn this back on
 * (scripts/assert-release-config.mjs). Settings shows a persistent
 * "Developer sign-in" banner while a PAT is in use.
 */
export function devSignInEnabled(isDev: boolean = __DEV__): boolean {
  return isDev === true;
}

export type DevSignInResult =
  | { ok: true; projectId: number }
  | { ok: false; error: 'empty' | 'disabled' | 'not_connected' | 'rejected' | 'unreachable' | 'no_project' | 'storage' | 'unexpected' };

/** First project that is not suspended, or null. */
export function pickDefaultProject(projects: readonly { id: number; suspended?: boolean }[]): number | null {
  return resolveProjectSelection(
    projects
      .filter((p) => Number.isSafeInteger(p.id) && p.id > 0)
      .map((p) => ({ id: p.id, name: '', suspended: p.suspended === true })),
    null,
  );
}

export interface DevSignInOptions {
  /** Injectable transport (tests). */
  fetch?: BaseFetch;
  enabled?: boolean;
}

/**
 * Validates `rawToken` against the connected workspace and, on success,
 * stores it (SecureStore, device-only) and makes it the session credential.
 * Nothing is stored unless the server accepted the token.
 */
export async function signInWithPat(rawToken: string, options: DevSignInOptions = {}): Promise<DevSignInResult> {
  if (!(options.enabled ?? devSignInEnabled())) return { ok: false, error: 'disabled' };
  const token = rawToken.trim();
  if (token === '') return { ok: false, error: 'empty' };
  const origin = getSession().serverOrigin;
  // The slot this sign-in is for: every write and any cleanup targets it.
  const workspaceId = boundWorkspace();
  const sessionWorkspace = getSession().workspaceId;
  if (origin === null || workspaceId === null) return { ok: false, error: 'not_connected' };
  const stillHere = () =>
    getSession().serverOrigin === origin && boundWorkspace() === workspaceId && getSession().workspaceId === sessionWorkspace;

  const client = createApiClient({ origin, auth: { kind: 'token', token }, fetch: options.fetch });
  let projectId: number | null;
  try {
    projectId = pickDefaultProject(await fetchProjects(client));
  } catch (error) {
    if (isApiError(error)) {
      if (error.kind === 'http' && (error.status === 401 || error.status === 403)) {
        return { ok: false, error: 'rejected' };
      }
      if (error.kind === 'network' || error.kind === 'timeout') return { ok: false, error: 'unreachable' };
    }
    log.warn('developer sign-in failed', { error });
    return { ok: false, error: 'unexpected' };
  }
  if (projectId === null) return { ok: false, error: 'no_project' };

  // The workspace may have been switched while the request was in flight.
  if (!stillHere()) return { ok: false, error: 'not_connected' };

  try {
    await setSecureItemIn(workspaceId, SECURE_KEYS.pat, token);
    await setSecureItemIn(workspaceId, SECURE_KEYS.credentialKind, 'pat');
    await setSecureItemIn(workspaceId, SECURE_KEYS.projectSelected, String(projectId));
  } catch (error) {
    log.warn('developer sign-in could not store the token', { error });
    await Promise.allSettled([
      deleteSecureItemIn(workspaceId, SECURE_KEYS.pat),
      deleteSecureItemIn(workspaceId, SECURE_KEYS.credentialKind),
      deleteSecureItemIn(workspaceId, SECURE_KEYS.projectSelected),
    ]);
    return { ok: false, error: 'storage' };
  }
  // Switched while storing: the token stays in its own slot (which opens
  // signed in) and never becomes the credential of the workspace now in use.
  if (!stillHere()) return { ok: false, error: 'not_connected' };

  setCredential({ kind: 'pat', token });
  setSession({ projectId, endReason: null, lastOriginHint: null, returnWorkspaceId: null });
  await activateSignedInWorkspace(workspaceId);
  return { ok: true, projectId };
}
