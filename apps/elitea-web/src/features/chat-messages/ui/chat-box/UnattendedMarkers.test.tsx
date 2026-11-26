import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ranInFullAuto } from '@/shared/lib/localWork';

import { stoppedAtStepLimit, unattendedMarksOf } from '../../lib/unattendedMarks';
import { ContinuedAutomaticallyLabel, FullAutoMarker, StepLimitNotice } from './UnattendedMarkers';

describe('the "Ran in Full auto" marker (issue 3079)', () => {
  it('is set only by a meta.local_work.full_auto record', () => {
    const meta = { local_work: { full_auto: { answered: 2, asks: [] } } };
    expect(ranInFullAuto(meta)).toBe(true);
    expect(unattendedMarksOf(meta)).toEqual({ fullAuto: true });
    for (const other of [undefined, null, {}, { local_work: {} }, { local_work: { full_auto: true } }, { local_work: { full_auto: [] } }, { full_auto: { answered: 1 } }]) {
      expect(ranInFullAuto(other)).toBe(false);
    }
  });

  it('renders the marker text', () => {
    renderWithTheme(<FullAutoMarker />);
    expect(screen.getByTestId('chat-message-full-auto')).toHaveTextContent('Ran in Full auto');
  });
});

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
