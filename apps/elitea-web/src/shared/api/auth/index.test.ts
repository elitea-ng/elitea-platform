/**
 * Public-surface smoke: the auth barrel is what the app layer (R2) and the
 * callback route consume in Wave 1/2 — its named exports must stay wired.
 */
import { describe, expect, it } from 'vitest';

import * as auth from './index';

describe('auth public surface', () => {
  it('exposes the §5.4 flow pieces by name', () => {
    expect(auth.AUTH_MESSAGE_TYPE).toBe('elitea-auth-result');
    expect(auth.AUTH_STATE_PARAM).toBe('auth_state');
    expect(auth.AUTH_CALLBACK_PATH).toBe('/auth-callback');
    expect(auth.LOGOUT_PATH).toBe('/auth/logout');
    expect(auth.VERIFY_SESSION_PATH).toBe('/social/author/');
    expect(typeof auth.createAuthPopupController).toBe('function');
    expect(typeof auth.completeAuthCallback).toBe('function');
    expect(typeof auth.createVerifySession).toBe('function');
    expect(typeof auth.performLogout).toBe('function');
    expect(typeof auth.sendAuthResult).toBe('function');
    expect(typeof auth.createBroadcastChannel).toBe('function');
    expect(typeof auth.isAuthResultMessage).toBe('function');
    expect(auth.AuthPopupError).toBeDefined();
  });
});
