import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ActionView } from './ActionView';

const OK = JSON.stringify({ status: 'ok', message: 'Pull request #7 opened', pull_request: 7, url: 'https://github.com/o/r/pull/7', branch: 'b', base: 'main', files: 2 });

describe('ActionView', () => {
  it('links the opened pull request', () => {
    renderWithTheme(<ActionView action={{ name: 'open_pull_request', toolOutputs: OK }} />);
    const link = screen.getByTestId('chat-tool-action-pull-request');
    expect(link).toHaveAttribute('href', 'https://github.com/o/r/pull/7');
    expect(link).toHaveTextContent('Open pull request #7');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  });

  it('links a merge request and a pushed branch', () => {
    const mr = JSON.stringify({ status: 'ok', pull_request: 4, forge: 'gitlab', url: 'https://gitlab.example.com/g/r/-/merge_requests/4' });
    const { unmount } = renderWithTheme(<ActionView action={{ name: 'open_pull_request', toolOutputs: mr }} />);
    expect(screen.getByTestId('chat-tool-action-pull-request')).toHaveTextContent('Open merge request !4');
    unmount();
    const pushed = JSON.stringify({ status: 'ok', branch: 'cloud_v2', url: 'https://github.com/o/r/tree/cloud_v2' });
    renderWithTheme(<ActionView action={{ name: 'git_push', toolOutputs: pushed }} />);
    expect(screen.getByTestId('chat-tool-action-pull-request')).toHaveTextContent('Open branch cloud_v2');
  });
});
