import { apiBaseUrl, isSameOrigin, isSameOriginApiPath, originOf, resolveApiPath } from './paths';

describe('isSameOriginApiPath', () => {
  it.each([
    '/api/v2/executions/1/abc/events',
    '/api/v2/executions/42/0b6f6c1e-5d2a-4a7e-9b1d-3c1f2e4d5a6b/events',
    `/api/v2/executions/1/${'a'.repeat(512)}/events`,
  ])('accepts %s', (path) => {
    expect(isSameOriginApiPath(path)).toBe(true);
  });

  it.each([
    ['absolute URL', 'https://evil/api/v2/executions/1/abc/events'],
    ['scheme-relative', '//evil/api/v2/executions/1/abc/events'],
    ['dot-dot segment', '/api/v2/../x'],
    ['dot segment', '/api/v2/./x'],
    ['outside the API prefix', '/auth/logout'],
    ['prefix without slash', '/api/v2'],
    ['look-alike prefix', '/api/v2evil/x'],
    ['CR', '/api/v2/x\r\nHost: evil'],
    ['LF', '/api/v2/x\nfoo'],
    ['backslash', '/api/v2/x\\..\\y'],
    ['empty segment', '/api/v2//x'],
    ['query', '/api/v2/x?cursor=1'],
    ['fragment', '/api/v2/x#y'],
    ['percent-encoding', '/api/v2/%2e%2e/x'],
    ['segment over 512 bytes', `/api/v2/executions/1/${'a'.repeat(513)}/events`],
    ['non-ASCII', '/api/v2/exécutions'],
    ['not a string', 42 as unknown as string],
  ])('rejects %s', (_label, path) => {
    expect(isSameOriginApiPath(path)).toBe(false);
  });
});

describe('origin helpers', () => {
  it('normalises case and default ports', () => {
    expect(originOf('HTTPS://AI.Example.com:443/x')).toBe('https://ai.example.com');
    expect(originOf('http://localhost:8080/api')).toBe('http://localhost:8080');
    expect(originOf('https://[::1]:8443')).toBe('https://[::1]:8443');
  });

  it('rejects userinfo and non-http schemes', () => {
    expect(originOf('https://user@evil.example/x')).toBeNull();
    expect(originOf('javascript:alert(1)')).toBeNull();
    expect(originOf('/relative')).toBeNull();
  });

  it('compares origins exactly', () => {
    expect(isSameOrigin('https://ai.example.com/api/v2/x', 'https://ai.example.com')).toBe(true);
    expect(isSameOrigin('https://ai.example.com.evil/x', 'https://ai.example.com')).toBe(false);
    expect(isSameOrigin('https://ai.example.com:8443/x', 'https://ai.example.com')).toBe(false);
  });

  it('resolves only valid API paths', () => {
    expect(resolveApiPath('https://ai.example.com', '/api/v2/executions/1/abc/events')).toBe(
      'https://ai.example.com/api/v2/executions/1/abc/events',
    );
    expect(resolveApiPath('https://ai.example.com', '//evil/api/v2/x')).toBeNull();
  });

  it('builds the REST base URL', () => {
    expect(apiBaseUrl('https://ai.example.com')).toBe('https://ai.example.com/api/v2');
    expect(() => apiBaseUrl('ai.example.com')).toThrow();
  });
});
