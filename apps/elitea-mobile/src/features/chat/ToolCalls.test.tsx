import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen } from '@testing-library/react-native';
import * as Clipboard from 'expo-clipboard';
import type { ReactNode } from 'react';

import { ToolActivity, formatDuration, groupByAgent, toolLabel, toolsSummary } from '@/features/chat/ToolCalls';
import { ToolDetailSheet } from '@/features/chat/ToolDetailSheet';
import type { ToolStep } from '@/features/chat/toolActivity';

jest.mock('expo-clipboard', () => ({ setStringAsync: jest.fn(async () => true) }));

function step(id: string, extra: Partial<ToolStep> = {}): ToolStep {
  return {
    id,
    name: 'list_issues',
    displayName: 'GitHub',
    toolkit: 'github',
    parentAgent: null,
    status: 'done',
    startedAt: 1_000,
    endedAt: 2_500,
    inputs: '{"state":"open"}',
    inputsTruncated: false,
    output: '{"items":2}',
    outputTruncated: false,
    partial: false,
    error: null,
    chunks: null,
    ...extra,
  };
}

describe('tool summaries', () => {
  it('counts the calls and the wall time they covered', () => {
    expect(toolsSummary([step('a')])).toBe('Used 1 tool · 1.5s');
    expect(toolsSummary([step('a'), step('b', { startedAt: 2_000, endedAt: 5_200 })])).toBe('Used 2 tools · 4.2s');
    expect(toolsSummary([step('a', { startedAt: null })])).toBe('Used 1 tool');
    expect(formatDuration(450)).toBe('450ms');
    expect(formatDuration(65_000)).toBe('1m 5s');
  });

  it('groups by the sub-agent that ran the call, in first-seen order', () => {
    const groups = groupByAgent([step('a', { parentAgent: 'r' }), step('b'), step('c', { parentAgent: 'r' })]);
    expect(groups.map((g) => [g.agent, g.steps.map((s) => s.id)])).toEqual([
      ['r', ['a', 'c']],
      [null, ['b']],
    ]);
  });
});

describe('ToolActivity', () => {
  it('is collapsed when settled and opens a chip on tap', async () => {
    const onOpen = jest.fn();
    await render(<ToolActivity steps={[step('a'), step('b', { status: 'error', name: 'read_file', displayName: null, toolkit: null })]} onOpen={onOpen} />);
    expect(screen.queryByLabelText('GitHub · list_issues, done')).toBeNull();
    await fireEvent.press(screen.getByLabelText('Used 2 tools · 1.5s'));
    expect(screen.getByLabelText('GitHub · list_issues, done')).toBeTruthy();
    await fireEvent.press(screen.getByLabelText('read_file, failed'));
    expect(onOpen).toHaveBeenCalledWith(expect.objectContaining({ id: 'b' }));
  });

  it('is expanded while the answer streams, with the running call labelled', async () => {
    await render(<ToolActivity steps={[step('a', { status: 'running', endedAt: null, parentAgent: 'researcher' })]} streaming onOpen={jest.fn()} />);
    expect(screen.getByLabelText('GitHub · list_issues, running')).toBeTruthy();
    expect(screen.getByText('researcher')).toBeTruthy();
  });

  it('says what a paused call waits for: an approval or an answer', async () => {
    await render(
      <ToolActivity
        steps={[
          step('a', { status: 'waiting', waitingFor: 'approval', endedAt: null }),
          step('b', { status: 'waiting', waitingFor: 'answer', endedAt: null, name: 'ask_user', displayName: null, toolkit: null }),
        ]}
        streaming
        onOpen={jest.fn()}
      />,
    );
    expect(screen.getByLabelText('GitHub · list_issues, waiting for approval')).toBeTruthy();
    expect(screen.getByLabelText('ask_user, waiting for your answer')).toBeTruthy();
  });

  it('renders nothing without calls', async () => {
    await render(<ToolActivity steps={[]} onOpen={jest.fn()} />);
    expect(screen.toJSON()).toBeNull();
  });
});

describe('ToolDetailSheet', () => {
  const queries = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={queries}>{children}</QueryClientProvider>;

  it('shows the input pretty-printed, the output on its tab, and copies the shown text', async () => {
    await render(<ToolDetailSheet step={step('a')} projectId={7} onClose={jest.fn()} />, { wrapper });
    expect(screen.getByText('{\n  "state": "open"\n}')).toBeTruthy();
    await fireEvent.press(screen.getByRole('tab', { name: 'Output' }));
    expect(screen.getByText('{\n  "items": 2\n}')).toBeTruthy();
    await fireEvent.press(screen.getByLabelText('Copy output'));
    expect(Clipboard.setStringAsync).toHaveBeenCalledWith('{\n  "items": 2\n}');
  });

  it('says when the output was cut or did not all arrive', async () => {
    await render(<ToolDetailSheet step={step('a', { outputTruncated: true, partial: true })} projectId={7} onClose={jest.fn()} />, { wrapper });
    await fireEvent.press(screen.getByRole('tab', { name: 'Output' }));
    expect(screen.getByText('Only the first 8 KB is shown.')).toBeTruthy();
    expect(screen.getByText('Part of this output did not arrive.')).toBeTruthy();
  });

  it("opens a failed call on its error", async () => {
    await render(<ToolDetailSheet step={step('a', { status: 'error', error: 'file not found', output: null })} projectId={7} onClose={jest.fn()} />, { wrapper });
    expect(screen.getByText('file not found')).toBeTruthy();
  });
});

describe('toolLabel', () => {
  // `metadata.display_name` is the TOOLKIT's display name in the SDK
  // (elitea_sdk sets it per toolkit: "Skills", "Smart Tools Selection"), as a
  // real openapi call on the 4.2.0 stack shows: toolkit_name "MockTools",
  // display_name "Mock Tools", tool_name "mock_tool_status".
  it('names the tool itself, under its toolkit', () => {
    expect(toolLabel({ name: 'mock_tool_status', displayName: 'Mock Tools', toolkit: 'MockTools' })).toBe('Mock Tools · mock_tool_status');
    expect(toolLabel({ name: 'mock_tool_status', displayName: null, toolkit: 'MockTools' })).toBe('MockTools · mock_tool_status');
    expect(toolLabel({ name: 'read_file', displayName: null, toolkit: null })).toBe('read_file');
  });
});
