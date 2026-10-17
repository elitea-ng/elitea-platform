import { describe, expect, it } from 'vitest';

import { LOCAL_WORK_SOURCE, isLocalWorkConversation, lastAnswerRanOnDesktop, localWorkFolderName, localWorkMeta, workspaceExecutorOf } from './localWork';

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

  it('reads where a thread runs; absent or unknown means local', () => {
    expect(workspaceExecutorOf({ workspace_executor: 'cloud' })).toBe('cloud');
    expect(workspaceExecutorOf({ workspace_executor: 'returning' })).toBe('returning');
    expect(workspaceExecutorOf({ workspace_executor: 'local' })).toBe('local');
    expect(workspaceExecutorOf({ workspace_executor: 'something-else' })).toBe('local');
    expect(workspaceExecutorOf({})).toBe('local');
    expect(workspaceExecutorOf(undefined)).toBe('local');
  });

  it('knows when the newest answer ran on a desktop (meta or metadata), whatever came before it', () => {
    const desktop = { role: 'assistant', meta: { executed_by: 'desktop' } };
    const cloud = { role: 'assistant', metadata: { executed_by: 'cloud_executor' } };
    const question = { role: 'user', meta: {} };
    expect(lastAnswerRanOnDesktop([cloud, question, desktop, question])).toBe(true);
    expect(lastAnswerRanOnDesktop([desktop, question, cloud])).toBe(false);
    expect(lastAnswerRanOnDesktop([{ role: 'assistant', metadata: { executed_by: 'desktop' } }])).toBe(true);
    expect(lastAnswerRanOnDesktop([question])).toBe(false);
    expect(lastAnswerRanOnDesktop([])).toBe(false);
    expect(lastAnswerRanOnDesktop([null, { role: 'assistant' }])).toBe(false);
  });
});
