import { openedPullRequest } from '@/features/chat/ToolCalls';

// A cloud thread's open_pull_request (ADR-0034 decision 10): its row opens the pull request.

const ok = JSON.stringify({ status: 'ok', message: 'Pull request #7 opened', pull_request: 7, url: 'https://github.com/o/r/pull/7' });

describe('openedPullRequest', () => {
  it('reads the opened pull request of a finished open_pull_request', () => {
    expect(openedPullRequest({ name: 'open_pull_request', status: 'done', output: ok })).toEqual({ url: 'https://github.com/o/r/pull/7', number: 7 });
  });

  it('links nothing else', () => {
    expect(openedPullRequest({ name: 'open_pull_request', status: 'running', output: ok })).toBeNull();
    expect(openedPullRequest({ name: 'create_pull_request', status: 'done', output: ok })).toBeNull();
    expect(openedPullRequest({ name: 'open_pull_request', status: 'done', output: JSON.stringify({ status: 'partial', url: 'https://x' }) })).toBeNull();
    expect(openedPullRequest({ name: 'open_pull_request', status: 'done', output: JSON.stringify({ status: 'ok', url: 'http://x' }) })).toBeNull();
    expect(openedPullRequest({ name: 'open_pull_request', status: 'done', output: 'not json' })).toBeNull();
  });
});
