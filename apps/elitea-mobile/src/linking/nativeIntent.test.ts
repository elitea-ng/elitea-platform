import { isOAuthRedirect, rewriteSystemPath } from '@/linking/nativeIntent';

describe('rewriteSystemPath', () => {
  const callback = 'ca.zefir.agent:/oauth/callback?code=abc&state=xyz&iss=https%3A%2F%2Fai.example.com';

  it('swallows the OAuth redirect while the app is running', () => {
    expect(rewriteSystemPath(callback, false)).toBeNull();
    expect(rewriteSystemPath('/oauth/callback?code=abc', false)).toBeNull();
  });

  it('sends a cold-start redirect to the app start', () => {
    expect(rewriteSystemPath(callback, true)).toBe('/');
  });

  it('passes every other URL through', () => {
    const connect = 'agentzefir://connect?server=https%3A%2F%2Fai.example.com';
    expect(rewriteSystemPath(connect, false)).toBe(connect);
    expect(rewriteSystemPath('/settings', true)).toBe('/settings');
    expect(isOAuthRedirect('/oauth/callbacks')).toBe(false);
  });
});
