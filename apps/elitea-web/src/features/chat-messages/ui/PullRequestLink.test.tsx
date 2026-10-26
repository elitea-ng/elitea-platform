import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ActionView } from './ActionView';
import { openedPullRequest } from './PullRequestLink';

const OK = JSON.stringify({ status: 'ok', message: 'Pull request #7 opened', pull_request: 7, url: 'https://github.com/o/r/pull/7', branch: 'b', base: 'main', files: 2 });

describe('openedPullRequest', () => {
  it('reads an opened pull request of open_pull_request only', () => {
    expect(openedPullRequest('open_pull_request', OK)).toEqual({ url: 'https://github.com/o/r/pull/7', number: 7 });
    expect(openedPullRequest('create_pull_request', OK)).toBeUndefined();
    expect(openedPullRequest('open_pull_request', JSON.stringify({ status: 'partial', url: 'https://github.com/o/r/pull/7' }))).toBeUndefined();
    expect(openedPullRequest('open_pull_request', JSON.stringify({ status: 'ok', url: 'javascript:alert(1)' }))).toBeUndefined();
    expect(openedPullRequest('open_pull_request', 'not json')).toBeUndefined();
  });
});

describe('ActionView', () => {
  it('links the opened pull request', () => {
    renderWithTheme(<ActionView action={{ name: 'open_pull_request', toolOutputs: OK }} />);
    const link = screen.getByTestId('chat-tool-action-pull-request');
    expect(link).toHaveAttribute('href', 'https://github.com/o/r/pull/7');
    expect(link).toHaveTextContent('Open pull request #7');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  });
});
