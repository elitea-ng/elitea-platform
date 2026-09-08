import type { ReactElement } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import type { TestToolkitToolOutcome } from '../../api/toolkitTestRun';

import { TestToolResultPanel } from './TestToolResultPanel';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderPanel(ui: ReactElement) {
  return render(
    <ThemeProvider
      theme={theme}
      defaultMode={DEFAULT_COLOR_SCHEME}
    >
      {ui}
    </ThemeProvider>,
  );
}

function renderOutcome(outcome: TestToolkitToolOutcome | undefined, isRunning = false) {
  return renderPanel(
    <TestToolResultPanel
      outcome={outcome}
      isRunning={isRunning}
    />,
  );
}

describe('TestToolResultPanel', () => {
  it('renders nothing before the first run', () => {
    renderOutcome(undefined);
    expect(screen.queryByTestId('test-tool-result')).not.toBeInTheDocument();
  });

  it('shows a running state while the request is in flight', () => {
    renderOutcome(undefined, true);
    expect(screen.getByTestId('test-tool-result')).toHaveAttribute('data-status', 'running');
  });

  it('shows the tool’s own result payload, pretty-printed', () => {
    renderOutcome({ kind: 'ok', result: { branches: ['main', 'dev'] }, truncated: false });

    expect(screen.getByTestId('test-tool-result')).toHaveAttribute('data-status', 'ok');
    // The RAW result is what the legacy panel showed and what a person testing
    // a credential reads: the branch names have to be on screen.
    expect(screen.getByTestId('test-tool-result-payload').textContent).toContain('main');
    expect(screen.getByTestId('test-tool-result-payload').textContent).toContain('dev');
    expect(screen.queryByTestId('test-tool-result-truncated')).not.toBeInTheDocument();
  });

  it('shows a string result as it came, without JSON quoting', () => {
    renderOutcome({ kind: 'ok', result: 'plain text answer', truncated: true });

    expect(screen.getByTestId('test-tool-result-payload')).toHaveTextContent('plain text answer');
    expect(screen.getByTestId('test-tool-result-payload').textContent).not.toContain('"plain');
    expect(screen.getByTestId('test-tool-result-truncated')).toBeInTheDocument();
  });

  it('keeps the four refusals apart, each with the server’s own sentence', () => {
    const cases = [
      { outcome: { kind: 'toolError', message: 'the tool said no' }, status: 'toolError' },
      { outcome: { kind: 'unsupportedToolkit', message: 'this deployment cannot build it' }, status: 'unsupportedToolkit' },
      { outcome: { kind: 'unknownTool', message: 'no tool by that name' }, status: 'unknownTool' },
      { outcome: { kind: 'failure', message: 'indexer service not available' }, status: 'failure' },
    ] as const satisfies readonly { outcome: TestToolkitToolOutcome & { message: string }; status: string }[];

    for (const testCase of cases) {
      const { unmount } = renderOutcome(testCase.outcome);
      const panel = screen.getByTestId('test-tool-result');
      expect(panel).toHaveAttribute('data-status', testCase.status);
      expect(panel).toHaveTextContent(testCase.outcome.message);
      unmount();
    }
  });

  it('keeps the job id of a run that outlived the bounded wait', () => {
    renderOutcome({ kind: 'timeout', taskId: 'job-42', message: 'still running' });

    expect(screen.getByTestId('test-tool-result')).toHaveAttribute('data-status', 'timeout');
    // The run has NOT failed — it is still going server-side, and this id is
    // the only handle anyone has on it afterwards.
    expect(screen.getByTestId('test-tool-result-task-id')).toHaveTextContent('job-42');
  });

  it('omits the job-id line for a timeout the server named no task for', () => {
    renderOutcome({ kind: 'timeout', taskId: undefined, message: 'still running' });

    expect(screen.getByTestId('test-tool-result')).toHaveAttribute('data-status', 'timeout');
    expect(screen.queryByTestId('test-tool-result-task-id')).not.toBeInTheDocument();
  });
});
