import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { stoppedAtStepLimit } from '../../lib/unattendedMarks';
import { ContinuedAutomaticallyLabel, StepLimitNotice } from './UnattendedMarkers';

describe('unattended run markers (ADR-0045)', () => {
  it('says "Continued automatically" in the desktop wording, with the chain position when known', () => {
    const { unmount } = renderWithTheme(<ContinuedAutomaticallyLabel index={3} />);
    expect(screen.getByTestId('chat-message-continued-automatically')).toHaveTextContent('Continued automatically (3)');
    unmount();
    renderWithTheme(<ContinuedAutomaticallyLabel />);
    expect(screen.getByTestId('chat-message-continued-automatically')).toHaveTextContent(/^Continued automatically$/u);
  });

  it('shows the step-limit stop as an end state', () => {
    renderWithTheme(<StepLimitNotice />);
    expect(screen.getByTestId('chat-message-step-limit')).toHaveTextContent('The run stopped at its step limit.');
  });

  it('reads the step-limit stop from the stored mark or from the live final frame', () => {
    expect(stoppedAtStepLimit({ stepLimitReached: true })).toBe(true);
    expect(stoppedAtStepLimit({ responseMetadata: { step_limit_reached: true } })).toBe(true);
    expect(stoppedAtStepLimit({ responseMetadata: { should_continue: true } })).toBe(false);
  });
});
