import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { AnsweringAgent } from './ChainNotices';

describe('AnsweringAgent (ADR-0050 decision 7f)', () => {
  it('names the agent that answers and where it runs', () => {
    renderWithTheme(<AnsweringAgent agent={{ name: 'Coder', participantId: 77, executor: 'local', position: 0, targets: 2, followed: false }} />);
    const line = screen.getByTestId('turn-agent');
    expect(line).toHaveTextContent('Coder · on this computer');
    expect(line).not.toHaveAttribute('data-followed');
  });

  it('says the thread routed the message to its lead rather than the composer’s agent (decision 7c, #1917)', () => {
    renderWithTheme(<AnsweringAgent agent={{ name: 'Reviewer', participantId: 88, executor: 'cloud', position: 0, targets: 1, followed: true }} />);
    const line = screen.getByTestId('turn-agent');
    expect(line).toHaveTextContent('Reviewer answers: the thread routed this message to it · in the cloud, conversation text only');
    expect(line).toHaveAttribute('data-followed', 'true');
    expect(line).toHaveAttribute('data-executor', 'cloud');
  });

  it('shows nothing for a turn of a thread with one agent', () => {
    const { container } = renderWithTheme(<AnsweringAgent agent={undefined} />);
    expect(container).toBeEmptyDOMElement();
  });
});
