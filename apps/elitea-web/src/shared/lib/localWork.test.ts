import { describe, expect, it } from 'vitest';

import { LOCAL_WORK_SOURCE, isLocalWorkConversation, localWorkFolderName, localWorkMeta } from './localWork';

describe('localWork', () => {
  it('recognises the Local work source only', () => {
    expect(isLocalWorkConversation({ source: LOCAL_WORK_SOURCE })).toBe(true);
    expect(isLocalWorkConversation({ source: 'elitea' })).toBe(false);
    expect(isLocalWorkConversation({})).toBe(false);
    expect(isLocalWorkConversation(undefined)).toBe(false);
  });

  it('round-trips the folder name through meta and ignores anything else', () => {
    expect(localWorkFolderName(localWorkMeta('api'))).toBe('api');
    expect(localWorkFolderName({ local_work: { folder_name: '  ' } })).toBeUndefined();
    expect(localWorkFolderName({ local_work: 'api' })).toBeUndefined();
    expect(localWorkFolderName(undefined)).toBeUndefined();
  });
});
