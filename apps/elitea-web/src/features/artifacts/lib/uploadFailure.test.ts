import { describe, expect, it } from 'vitest';

import { describeUploadFailure } from './uploadFailure';

const BLOCKED = 'The upload was blocked before it reached Elitea (HTTP 403 from a proxy). Ask your administrator.';

describe('describeUploadFailure', () => {
  it('names a WAF/proxy HTML 403 instead of a bare failure', () => {
    expect(describeUploadFailure({
      kind: 'http', status: 403, url: '/x', body: '<html><body>Request blocked</body></html>',
    })).toBe(BLOCKED);
  });

  it('treats an empty or foreign JSON body as not ours', () => {
    expect(describeUploadFailure({ kind: 'http', status: 403, url: '/x', body: undefined })).toBe(BLOCKED);
    expect(describeUploadFailure({ kind: 'http', status: 403, url: '/x', body: { blocked: true } })).toBe(BLOCKED);
  });

  it("surfaces the server's own envelope message", () => {
    expect(describeUploadFailure({
      kind: 'http', status: 409, url: '/x',
      body: { error: { code: 'AlreadyExists', message: 'key already exists; pass overwrite=true to replace it' } },
    })).toBe('key already exists; pass overwrite=true to replace it');
    expect(describeUploadFailure({ kind: 'http', status: 403, url: '/x', body: { error: 'forbidden' } })).toBe('forbidden');
  });

  it('describes network failures', () => {
    expect(describeUploadFailure({ kind: 'network', url: '/x', message: 'm', cause: null }))
      .toBe('The connection failed before the upload finished.');
  });
});
