/** A refused first send's thread: retried in, or left with a new client id for another agent or version (#3077). */
import { describe, expect, it } from 'vitest';

import { retryTarget } from './sendRetry';

const pending = { key: '42:5:9', id: '77', ready: true };

describe('retryTarget', () => {
  it('retries the same agent and version in the thread the refused send created', () => {
    expect(retryTarget(pending, true, '42:5:9')).toEqual({ reuse: pending, abandoned: false });
  });

  it('leaves it for another agent or version, so that send gets a new client id', () => {
    expect(retryTarget(pending, true, '42:6:10')).toEqual({ reuse: null, abandoned: true });
  });

  it('leaves nothing in an existing thread, or with nothing pending', () => {
    expect(retryTarget(pending, false, '42:5:9')).toEqual({ reuse: null, abandoned: false });
    expect(retryTarget(null, true, '42:5:9')).toEqual({ reuse: null, abandoned: false });
  });
});
