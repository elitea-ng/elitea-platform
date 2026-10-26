import { describe, expect, it } from 'vitest';

import { forgeLinkOf } from './forgeLink';

const answer = (fields: Record<string, unknown>): string => JSON.stringify({ status: 'ok', ...fields });

describe('forgeLinkOf', () => {
  it('reads a pull request, a merge request and a pushed branch', () => {
    expect(forgeLinkOf('open_pull_request', answer({ pull_request: 7, url: 'https://github.com/o/r/pull/7' }))).toEqual({
      kind: 'pull_request',
      url: 'https://github.com/o/r/pull/7',
      number: 7,
    });
    expect(forgeLinkOf('open_pull_request', answer({ pull_request: 4, forge: 'gitlab', url: 'https://gitlab.example.com/g/r/-/merge_requests/4' }))).toEqual({
      kind: 'merge_request',
      url: 'https://gitlab.example.com/g/r/-/merge_requests/4',
      number: 4,
    });
    expect(forgeLinkOf('git_push', answer({ branch: 'cloud_v2', url: 'https://github.com/o/r/tree/cloud_v2' }))).toEqual({
      kind: 'branch',
      url: 'https://github.com/o/r/tree/cloud_v2',
      branch: 'cloud_v2',
    });
  });

  it('links nothing else', () => {
    expect(forgeLinkOf('create_pull_request', answer({ url: 'https://github.com/o/r/pull/7' }))).toBeUndefined();
    expect(forgeLinkOf('open_pull_request', JSON.stringify({ status: 'partial', url: 'https://github.com/o/r/pull/7' }))).toBeUndefined();
    expect(forgeLinkOf('open_pull_request', answer({ url: 'javascript:alert(1)' }))).toBeUndefined();
    expect(forgeLinkOf('git_push', answer({ url: 'http://github.com/o/r/tree/x', branch: 'x' }))).toBeUndefined();
    expect(forgeLinkOf('open_pull_request', 'not json')).toBeUndefined();
    expect(forgeLinkOf('open_pull_request', { status: 'ok' })).toBeUndefined();
  });
});
