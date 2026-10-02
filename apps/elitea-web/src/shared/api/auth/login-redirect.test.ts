import { describe, expect, it } from 'vitest';

import { FORM_LOGIN_PATH, SSO_LOGIN_PATH } from './constants';
import { authPlaneFromProbeStatus, buildLoginUrl, loginPathForPlane } from './login-redirect';

describe('authPlaneFromProbeStatus', () => {
  it('reads a 404 from /forward-auth/info as the Form plane', () => {
    // The Form composition mounts no /forward-auth/info at all
    // (internal/api/production_router.go mounts one plane or the other), so
    // "that endpoint is not there" is the only signal that identifies it.
    expect(authPlaneFromProbeStatus(404)).toBe('form');
  });

  it('does NOT read a 401 as the Form plane', () => {
    // A 401 is the OIDC plane answering "no session". Treating it as Form
    // would send an OIDC deployment to a login path it does not serve.
    expect(authPlaneFromProbeStatus(401)).toBe('oidc');
  });

  it.each([200, 403, 500, undefined])('treats %s as the OIDC plane', (status) => {
    expect(authPlaneFromProbeStatus(status)).toBe('oidc');
  });
});

describe('loginPathForPlane', () => {
  it('sends the Form plane to the transaction opener, not the form itself', () => {
    // /forward-auth/auth_form/login 400s without a transaction id; only
    // /forward-auth/login creates one (browserauth/handler.go beginLogin).
    expect(loginPathForPlane('form')).toBe(FORM_LOGIN_PATH);
    expect(FORM_LOGIN_PATH).toBe('/forward-auth/login');
  });

  it('sends the SSO plane to the sign-in page, not straight to OIDC', () => {
    // A fixed /forward-auth/auth_oidc/login could never reach a SAML provider.
    // The sign-in page redirects to the one usable provider, or offers both.
    expect(loginPathForPlane('oidc')).toBe(SSO_LOGIN_PATH);
    expect(SSO_LOGIN_PATH).toBe('/forward-auth/login');
  });
});

describe('buildLoginUrl', () => {
  it('carries the caller back to where they were', () => {
    expect(buildLoginUrl('form', '/app/chat')).toBe('/forward-auth/login?target_to=%2Fapp%2Fchat');
  });

  it('encodes a query string so it survives the round trip', () => {
    // Un-encoded, the `&` would terminate target_to and the rest would arrive
    // as separate parameters of the login URL itself.
    expect(buildLoginUrl('form', '/app/chat?a=1&b=2')).toBe(
      '/forward-auth/login?target_to=%2Fapp%2Fchat%3Fa%3D1%26b%3D2',
    );
  });

  it('forces a leading slash', () => {
    // CanonicalReturnTarget rejects anything that is not a same-origin
    // absolute path and falls back to default_login silently, losing the
    // user's place rather than failing loudly.
    expect(buildLoginUrl('form', 'app/chat')).toBe('/forward-auth/login?target_to=%2Fapp%2Fchat');
  });

  it('uses the sign-in page for the SSO plane and keeps the return target', () => {
    expect(buildLoginUrl('oidc', '/app/')).toBe('/forward-auth/login?target_to=%2Fapp%2F');
  });
});
