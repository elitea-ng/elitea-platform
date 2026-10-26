import { openedForgeLink } from '@/features/chat/ToolCalls';

// A cloud thread's git_push and open_pull_request (ADR-0034 decision 10): their rows open the
// pushed branch, the GitHub pull request or the GitLab merge request.

const ok = JSON.stringify({ status: 'ok', message: 'Pull request #7 opened', forge: 'github', pull_request: 7, url: 'https://github.com/o/r/pull/7' });

describe('openedForgeLink', () => {
  it('reads the opened pull request of a finished open_pull_request', () => {
    expect(openedForgeLink({ name: 'open_pull_request', status: 'done', output: ok })).toEqual({ url: 'https://github.com/o/r/pull/7', label: 'Open pull request #7' });
  });

  it('names a GitLab merge request and a pushed branch', () => {
    const mr = JSON.stringify({ status: 'ok', forge: 'gitlab', pull_request: 4, url: 'https://gitlab.example.com/g/p/-/merge_requests/4' });
    expect(openedForgeLink({ name: 'open_pull_request', status: 'done', output: mr })).toEqual({
      url: 'https://gitlab.example.com/g/p/-/merge_requests/4',
      label: 'Open merge request !4',
    });
    const pushed = JSON.stringify({ status: 'ok', branch: 'elitea/fix', url: 'https://github.com/o/r/tree/elitea/fix', commits: 2 });
    expect(openedForgeLink({ name: 'git_push', status: 'done', output: pushed })).toEqual({ url: 'https://github.com/o/r/tree/elitea/fix', label: 'Open branch elitea/fix' });
  });

  it('links nothing else', () => {
    expect(openedForgeLink({ name: 'open_pull_request', status: 'running', output: ok })).toBeNull();
    expect(openedForgeLink({ name: 'create_pull_request', status: 'done', output: ok })).toBeNull();
    expect(openedForgeLink({ name: 'git_push', status: 'done', output: JSON.stringify({ status: 'ok', url: 'https://x' }) })).toBeNull();
    expect(openedForgeLink({ name: 'open_pull_request', status: 'done', output: JSON.stringify({ status: 'partial', url: 'https://x' }) })).toBeNull();
    expect(openedForgeLink({ name: 'open_pull_request', status: 'done', output: JSON.stringify({ status: 'ok', url: 'http://x' }) })).toBeNull();
    expect(openedForgeLink({ name: 'open_pull_request', status: 'done', output: 'not json' })).toBeNull();
  });
});
