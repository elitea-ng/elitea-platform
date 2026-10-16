import { ThemeProvider } from '@mui/material/styles';
import { render, screen, waitFor } from '@testing-library/react';
import type { ReactNode } from 'react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { TraceStepDetailContext } from '../../model/traceStepDetail';
import type { TraceStepLoader } from '../../model/traceStepDetail';

import { AskUserAnswerSummary } from './AskUserAnswerSummary';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function wrap(node: ReactNode, loader?: TraceStepLoader) {
  return render(
    <ThemeProvider theme={theme}>
      <TraceStepDetailContext value={loader}>{node}</TraceStepDetailContext>
    </ThemeProvider>,
  );
}

const QUESTIONS = { questions: [{ question: 'Which environment?' }, { question: 'Why?' }] };
const OUTPUT = 'User answered:\n- Which environment?: Staging\n- Why?: testing';

describe('AskUserAnswerSummary', () => {
  it('lists numbered questions with their answers from inline data', () => {
    const loader = vi.fn<TraceStepLoader>();
    wrap(
      <AskUserAnswerSummary
        toolActions={[{ name: 'ask_user', status: 'complete', toolInputs: QUESTIONS, toolOutputs: OUTPUT, traceStepId: 1, traceMessageGroupId: 2 }]}
      />,
      loader,
    );

    expect(screen.getByText('1. Which environment?')).toBeTruthy();
    expect(screen.getByText('Staging')).toBeTruthy();
    expect(screen.getByText('2. Why?')).toBeTruthy();
    expect(loader, 'inline data never triggers a fetch').not.toHaveBeenCalled();
  });

  it('fetches a reloaded step that carries no inline data', async () => {
    const loader = vi.fn<TraceStepLoader>().mockResolvedValue({ toolInputs: QUESTIONS, output: OUTPUT });
    wrap(
      <AskUserAnswerSummary toolActions={[{ name: 'ask_user', status: 'complete', traceStepId: 7, traceMessageGroupId: 9 }]} />,
      loader,
    );

    await waitFor(() => expect(screen.getByText('testing')).toBeTruthy());
    expect(loader).toHaveBeenCalledWith(7, 9);
  });

  it('renders nothing for a running ask_user call or no ask_user call', () => {
    const { container } = wrap(
      <AskUserAnswerSummary toolActions={[{ name: 'ask_user', status: 'processing', toolOutputs: OUTPUT }, { name: 'x', status: 'complete' }]} />,
    );
    expect(container.textContent).toBe('');
  });
});
