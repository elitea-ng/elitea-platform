import { screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { projectInstructionsOf, ranInFullAuto } from '@/shared/lib/localWork';

import { stoppedAtStepLimit, unattendedMarksOf } from '../../lib/unattendedMarks';
import { AnswerMarkers, ContinuedAutomaticallyLabel, FullAutoMarker, ProjectInstructionsMarker, StepLimitNotice } from './UnattendedMarkers';

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

describe('the read-only AGENTS.md marker of a Local work turn (issue 3130)', () => {
  it('is read from a meta.local_work.project_instructions record only', () => {
    const meta = { local_work: { project_instructions: { applied: false, files: ['AGENTS.md', 7] } } };
    expect(projectInstructionsOf(meta)).toEqual({ applied: false, files: ['AGENTS.md'] });
    expect(unattendedMarksOf(meta)).toEqual({ projectInstructions: { applied: false, files: ['AGENTS.md'] } });
    for (const other of [undefined, null, {}, { local_work: {} }, { local_work: { project_instructions: true } }, { local_work: { project_instructions: { files: [] } } }, { local_work: { project_instructions: { applied: true } } }]) {
      expect(projectInstructionsOf(other)).toBeUndefined();
    }
  });

  it('says whether the turn applied or ignored AGENTS.md, naming the files in its title', () => {
    const { unmount } = renderWithTheme(<ProjectInstructionsMarker applied={false} files={['AGENTS.md', 'apps/web/AGENTS.md']} />);
    const marker = screen.getByTestId('chat-message-project-instructions');
    expect(marker).toHaveTextContent('AGENTS.md ignored');
    expect(marker).toHaveAttribute('title', 'AGENTS.md, apps/web/AGENTS.md');
    unmount();
    renderWithTheme(<AnswerMarkers answer={{ projectInstructions: { applied: true, files: ['AGENTS.md'] } }} />);
    expect(screen.getByTestId('chat-message-project-instructions')).toHaveTextContent('AGENTS.md applied');
    expect(screen.queryByTestId('chat-message-full-auto')).toBeNull();
  });

  it('is not shown in the desktop build, which states it once in the Participants tab', () => {
    vi.stubEnv('MODE', 'desktop');
    try {
      renderWithTheme(<AnswerMarkers answer={{ projectInstructions: { applied: true, files: ['AGENTS.md'] } }} />);
      expect(screen.queryByTestId('chat-message-project-instructions')).toBeNull();
    } finally {
      vi.unstubAllEnvs();
    }
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
