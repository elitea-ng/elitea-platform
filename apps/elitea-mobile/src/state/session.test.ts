import {
  INITIAL_SESSION,
  __resetSessionForTests,
  getBearerToken,
  getSession,
  resetSession,
  setCredential,
  setSession,
  subscribeSession,
} from './session';

beforeEach(() => __resetSessionForTests());

describe('session store', () => {
  it('starts signed out and not hydrated', () => {
    expect(getSession()).toEqual(INITIAL_SESSION);
    expect(getBearerToken()).toBeNull();
  });

  it('notifies subscribers on change and not on a no-op patch', () => {
    const listener = jest.fn();
    subscribeSession(listener);
    setSession({ serverOrigin: 'https://a.example' });
    setSession({ serverOrigin: 'https://a.example' });
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it('keeps the bearer secret out of the observable state', () => {
    setCredential({ kind: 'pat', token: 'super-secret' });
    expect(getSession().credential).toEqual({ kind: 'pat' });
    expect(JSON.stringify(getSession())).not.toContain('super-secret');
    expect(getBearerToken()).toBe('super-secret');
  });

  it('reset clears the credential but keeps hydrated', () => {
    setSession({ hydrated: true, serverOrigin: 'https://a.example', projectId: 3 });
    setCredential({ kind: 'pat', token: 't' });
    resetSession({ endReason: 'user_logout', lastOriginHint: null });
    expect(getSession()).toEqual({ ...INITIAL_SESSION, hydrated: true, endReason: 'user_logout' });
    expect(getBearerToken()).toBeNull();
  });
});
