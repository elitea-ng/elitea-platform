import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { PipelineStatus } from '../../lib/flow-editor/constants/flowEditor.constants';
import type { GroupedTimelineStep } from '../../lib/run-state-timeline/timeline.types';
import { TimeLine } from './TimeLine';

const baseTime = new Date('2026-11-22T10:15:30Z').getTime();

const groupedSteps: GroupedTimelineStep[] = [
  {
    id: 'prepare',
    group_id: 'row:0',
    group_label: 'prepare',
    status: PipelineStatus.Completed,
    created_at: baseTime,
  },
  {
    id: 'sub_pipeline',
    group_id: 'application:call-1',
    group_label: 'Sub Pipeline',
    status: PipelineStatus.Completed,
    created_at: baseTime + 1000,
    is_nested: false,
  },
  {
    id: 'llm_plan',
    group_id: 'application:call-1',
    group_label: 'Sub Pipeline',
    status: PipelineStatus.Completed,
    created_at: baseTime + 2000,
    is_nested: true,
  },
  {
    id: 'tool_search',
    group_id: 'application:call-1',
    group_label: 'Sub Pipeline',
    status: PipelineStatus.Completed,
    created_at: baseTime + 3000,
    is_nested: true,
  },
  {
    id: 'finalize',
    group_id: 'row:4',
    group_label: 'finalize',
    status: PipelineStatus.Completed,
    created_at: baseTime + 4000,
  },
];

describe('TimeLine', () => {
  it('collapses grouped steps by default, shows count badge, and expands via group toggle and expand-all switch', () => {
    const onSelect = vi.fn();
    renderWithTheme(
      <TimeLine
        steps={groupedSteps}
        status={PipelineStatus.Completed}
        selectedStep={0}
        onSelect={onSelect}
      />,
    );

    expect(screen.getByTestId('pipeline-run-details-timeline-section')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-run-details-timeline-step-0')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-run-details-timeline-step-1')).toBeInTheDocument();
    expect(screen.queryByTestId('pipeline-run-details-timeline-step-2')).not.toBeInTheDocument();
    expect(screen.getByTestId('pipeline-run-details-timeline-group-count-1')).toHaveTextContent('3');

    fireEvent.click(screen.getByTestId('pipeline-run-details-timeline-group-toggle-1'));
    expect(screen.getByTestId('pipeline-run-details-timeline-step-2')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-run-details-timeline-step-3')).toBeInTheDocument();

    fireEvent.click(screen.getByTestId('pipeline-run-details-timeline-step-3'));
    expect(onSelect).toHaveBeenCalledWith(3);

    fireEvent.click(screen.getByTestId('pipeline-run-details-timeline-expand-all'));
    expect(screen.getByTestId('pipeline-run-details-timeline-step-2')).toBeInTheDocument();
  });

  it('hides the expand-all switch when no multi-step groups exist', () => {
    renderWithTheme(
      <TimeLine
        steps={[groupedSteps[0] as GroupedTimelineStep, groupedSteps[4] as GroupedTimelineStep]}
        status={PipelineStatus.Completed}
        selectedStep={0}
        onSelect={vi.fn()}
      />,
    );

    expect(screen.queryByTestId('pipeline-run-details-timeline-expand-all')).not.toBeInTheDocument();
  });

  it('renders live run status indicator while run is in progress', () => {
    const liveSteps: GroupedTimelineStep[] = [
      { id: 'fetch', status: PipelineStatus.Completed, created_at: baseTime },
      { id: 'analyze', status: PipelineStatus.InProgress, created_at: baseTime + 1000 },
    ];
    renderWithTheme(
      <TimeLine
        steps={liveSteps}
        status={PipelineStatus.InProgress}
        selectedStep={1}
        onSelect={vi.fn()}
      />,
    );

    expect(screen.getByText('Performing')).toBeInTheDocument();
    expect(screen.getByText('analyze:')).toBeInTheDocument();
  });

  it('marks continued failures ("Failed · continued") and renders support refs safely as text without HTML injection', () => {
    const htmlPayload = '<img src=x onerror=alert(1)>';
    const stepsWithContinuedFailure: GroupedTimelineStep[] = [
      {
        id: 'worker',
        group_id: 'map:worker:0',
        group_label: 'Worker',
        status: PipelineStatus.Completed,
        created_at: baseTime,
      },
      {
        id: 'worker_tool',
        group_id: 'map:worker:0',
        group_label: 'Worker',
        status: PipelineStatus.Completed,
        created_at: baseTime + 1000,
        state: {
          node_errors: [
            {
              node: 'worker_tool',
              handled: 'continue',
              input_step: 2,
              error: {
                support_ref: 'REF-42',
                message: htmlPayload,
              },
            },
          ],
        },
      },
    ];

    const { container } = renderWithTheme(
      <TimeLine
        steps={stepsWithContinuedFailure}
        status={PipelineStatus.Completed}
        selectedStep={0}
        onSelect={vi.fn()}
      />,
    );

    const stepDot = screen.getByTestId('pipeline-run-details-timeline-step-0');
    expect(stepDot).toHaveAttribute('data-status', 'failed-continued');

    const banner = screen.getByTestId('pipeline-run-details-continued-failures');
    expect(banner).toHaveTextContent('Failed · continued');
    expect(screen.getByTestId('pipeline-run-details-continued-failure-ref-0')).toHaveTextContent('REF-42');
    expect(banner).toHaveTextContent(htmlPayload);
    expect(container.querySelector('img')).toBeNull();
  });
});
