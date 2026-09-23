import { afterEach, expect, it } from 'vitest';
import { getAuthorizationReference } from './authorizationReference';
import { setAccessToken } from './storage';

const server = 'https://mcp.example/tools';
const reference = 'R'.repeat(43);
afterEach(() => { sessionStorage.clear(); localStorage.clear(); });

it('selects only a scoped, unexpired server authorization reference', () => {
  setAccessToken(server, 'browser-token', 3600, undefined, undefined, undefined, { project_id: '7', toolkit_id: '9', resource: server, authorization_reference: reference });
  expect(getAuthorizationReference('7', '9', server)).toBe(reference);
  expect(getAuthorizationReference('8', '9', server)).toBeUndefined();
  expect(getAuthorizationReference('7', '8', server)).toBeUndefined();
  expect(getAuthorizationReference('7', '9', 'https://other.example/tools')).toBeUndefined();
});

it('does not reuse a missing or expired reference or substitute browser tokens', () => {
  setAccessToken(server, 'browser-token', 3600, undefined, undefined, undefined, { project_id: '7', toolkit_id: '9', resource: server });
  expect(getAuthorizationReference('7', '9', server)).toBeUndefined();
  setAccessToken(server, 'browser-token', -1, undefined, undefined, undefined, { project_id: '7', toolkit_id: '9', resource: server, authorization_reference: reference });
  expect(getAuthorizationReference('7', '9', server)).toBeUndefined();
});
