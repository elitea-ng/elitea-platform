/**
 * Complete logout — §5.4 behaviour 7. The proof is the write-enumeration
 * mechanism: EVERY write made through the storage wrapper is tracked, and
 * after logout the write-set minus the cleared-set must be EMPTY — including
 * sessionStorage. A hardcoded key list could silently rot; this cannot.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { installWebStorageShim } from '../../../test/webstorage';

installWebStorageShim();

import {
  createStorage,
  disableStorageWriteTracking,
  enableStorageWriteTracking,
  trackedStorageWrites,
} from '../../lib/storage';

import { setNativeTransport } from '../nativeTransport';

import { performLogout } from './logout';

afterEach(() => {
  setNativeTransport(undefined);
  disableStorageWriteTracking();
  window.localStorage.clear();
  window.sessionStorage.clear();
});

function storageFor(area: 'local' | 'session'): Storage {
  return area === 'local' ? window.localStorage : window.sessionStorage;
}

describe('performLogout', () => {
  it('write-set minus cleared-set is EMPTY — every el.* write in BOTH areas is gone', () => {
    enableStorageWriteTracking();
    const local = createStorage('local');
    const session = createStorage('session');

    // The exact state the OLD logout leaked (slices/user.js:24-27 cleared
    // only the two permission keys), mapped into the el.* namespace:
    local.set('project.id', '42'); //            was elitea_ui.project.id — leaked
    local.set('project.name', 'Demo'); //        was elitea_ui.project.name — leaked
    local.setJSON('mcp.tokens.v1', [{ server: 'a', token: 't' }]); // was elitea_mcp_tokens_v1 — leaked
    local.set('tour.chat.completed', '1'); //    tour keys — leaked
    local.set('mode', 'dark');
    // State-scoped auth-result fallbacks (LOW-4): the sweep must clear every
    // state without enumerating them.
    local.setJSON('auth.result.9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d', { stale: true });
    local.setJSON('auth.result.11111111-2222-4333-8444-555555555555', { stale: true });
    session.set('project_permission', '["models.read"]'); // the two the old logout DID clear
    session.set('public_permission', '[]');
    session.set('auth.state', 'some-uuid');

    const writes = trackedStorageWrites();
    expect(writes.size).toBe(10);

    const redirect = vi.fn();
    performLogout({ redirect });

    const leaked = [...writes].filter((entry) => {
      const [area, underlyingKey] = entry.split(/:(.+)/, 2) as ['local' | 'session', string];
      return storageFor(area).getItem(underlyingKey) !== null;
    });
    expect(leaked).toEqual([]); // write-set − cleared-set = ∅
  });

  it('does not touch keys outside the el. namespace', () => {
    createStorage('local').set('project.id', '42');
    window.localStorage.setItem('other_app.pref', 'keep');
    window.sessionStorage.setItem('other_app.session', 'keep');
    performLogout({ redirect: vi.fn() });
    expect(window.localStorage.getItem('other_app.pref')).toBe('keep');
    expect(window.sessionStorage.getItem('other_app.session')).toBe('keep');
  });

  /**
   * `target_to` names a login entry point so the signed-out user actually
   * reaches a login screen (JRNY-004's third acceptance line). See
   * `logout.ts` for why that is behavioural parity with the old app rather
   * than an addition: the old edge supplied the same end state implicitly by
   * gating the SPA, which this stack does not do.
   *
   * DEFECT this value pins: the target used to be
   * `/auth/oidc/login`. `internal/api/router.go` registers that
   * path inside `if cfg.OIDCHandler != nil`, so a form-auth deployment does
   * not have it. The logout chain ended on chi's bare `404 page not found`,
   * with the user signed out and no link back. `/auth/login` is the
   * one login path that BOTH planes register, so it is the correct target.
   */
  const LOGOUT_URL = '/auth/logout?target_to=%2Fauth%2Flogin';

  it('hands the browser to the backend logout, targeted at the login screen', () => {
    const redirect = vi.fn();
    performLogout({ redirect, origin: 'https://app.example' });
    expect(redirect).toHaveBeenCalledExactlyOnceWith('https://app.example' + LOGOUT_URL);
  });

  it('percent-encodes the target so its leading slash cannot be read as a path segment', () => {
    const redirect = vi.fn();
    performLogout({ redirect, origin: 'https://app.example' });
    const url = new URL(String(redirect.mock.calls[0]?.[0]));
    expect(url.pathname).toBe('/auth/logout');
    expect(url.searchParams.get('target_to')).toBe('/auth/login');
  });

  it('targets a login path that a form-auth deployment also registers', () => {
    // Form-plane regression. The two planes are mutually exclusive, and
    // `production_router.go` mounts `browserauth` when `SessionHandler` is
    // nil. That plane registers `/login`, `/auth_form/login`,
    // `/auth_form/authorize`, `/logout` and `/auth_form/logout` under
    // `/auth`, and nothing under `/auth_oidc/`. A target inside
    // `/auth/oidc/` therefore ends the sign-out on a 404.
    //
    // The existing E2E stack is OIDC-only, so it cannot see this. The check
    // is the assertion below, not a live request.
    const redirect = vi.fn();
    performLogout({ redirect, origin: 'https://app.example' });
    const target = new URL(String(redirect.mock.calls[0]?.[0])).searchParams.get('target_to');
    expect(target).not.toContain('/auth_oidc/');
  });

  it('defaults the origin to the page origin', () => {
    const redirect = vi.fn();
    performLogout({ redirect });
    expect(redirect).toHaveBeenCalledExactlyOnceWith('http://localhost:3000' + LOGOUT_URL);
  });

  it('defaults the redirect to a window.location.href assignment', () => {
    // jsdom does not implement navigation; the assignment itself must not throw.
    expect(() => performLogout({ origin: 'http://localhost:3000' })).not.toThrow();
  });
});

describe('performLogout under a native transport', () => {
  it('signs out through the host instead of navigating to a URL the app cannot serve', () => {
    createStorage('local').set('project.id', '42');
    const logout = vi.fn<() => Promise<void>>().mockResolvedValue();
    setNativeTransport({
      accessToken: () => Promise.resolve('t'),
      refresh: () => Promise.resolve('refreshed'),
      signOut: vi.fn(),
      logout,
    });
    const redirect = vi.fn();

    performLogout({ redirect, origin: 'tauri://localhost' });

    expect(logout).toHaveBeenCalledTimes(1);
    expect(redirect).not.toHaveBeenCalled();
    expect(window.localStorage.getItem('el.project.id')).toBeNull();
  });
});
