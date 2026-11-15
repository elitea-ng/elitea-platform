import { isOAuthRedirect, rewriteSystemPath } from '@/linking/nativeIntent';

describe('rewriteSystemPath', () => {
  const callback = 'ai.elitea.app:/oauth/callback?code=abc&state=xyz&iss=https%3A%2F%2Fai.example.com';

  it('swallows the OAuth redirect while the app is running', () => {
    expect(rewriteSystemPath(callback, false)).toBeNull();
    expect(rewriteSystemPath('/oauth/callback?code=abc', false)).toBeNull();
  });

  it('sends a cold-start redirect to the app start', () => {
    expect(rewriteSystemPath(callback, true)).toBe('/');
  });

  it('passes every other URL through', () => {
    const connect = 'elitea://connect?server=https%3A%2F%2Fai.example.com';
    expect(rewriteSystemPath(connect, false)).toBe(connect);
    expect(rewriteSystemPath('/settings', true)).toBe('/settings');
    expect(isOAuthRedirect('/oauth/callbacks')).toBe(false);
  });
});

describe('rewriteSystemPath: Marketplace links', () => {
  it('opens the Marketplace tab for the web address and the app scheme', () => {
    expect(rewriteSystemPath('/marketplace', false)).toBe('/discover');
    expect(rewriteSystemPath('elitea://marketplace', true)).toBe('/discover');
    expect(rewriteSystemPath('/marketplace?kind=skills', false)).toBe('/discover');
  });

  it('opens an agent sheet for a listing link and for the old shared ?agentId= link', () => {
    expect(rewriteSystemPath('/marketplace/agents/42', false)).toBe('/discover/42');
    expect(rewriteSystemPath('elitea://marketplace/agents/42', false)).toBe('/discover/42');
    expect(rewriteSystemPath('/elitea-catalog?tab=agents&agentId=7', false)).toBe('/discover/7');
    expect(rewriteSystemPath('/agents-hub?agentId=7', false)).toBe('/discover/7');
  });

  it('opens the tab, never a route that does not exist, for a skill or an id that is not a number', () => {
    expect(rewriteSystemPath('/marketplace/skills/9', false)).toBe('/discover');
    expect(rewriteSystemPath('/marketplace/agents/not-a-number', false)).toBe('/discover');
    expect(rewriteSystemPath('/elitea-catalog?tab=skills', false)).toBe('/discover');
    expect(rewriteSystemPath('/agents-hub', false)).toBe('/discover');
  });

  it('leaves another scheme alone', () => {
    expect(rewriteSystemPath('https://evil.example/marketplace', false)).toBe('https://evil.example/marketplace');
  });
});
