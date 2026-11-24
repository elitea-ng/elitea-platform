import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { NodeHeightMap, PipelineNodeTypes } from '../../lib/flow-editor/constants/flowEditor.constants';
import {
  getNodeCardV2Status,
  getNodeCardV2Summary,
  isMiniCardZoom,
  NodeCardView,
} from './index';

describe('nodeCardV2 helpers', () => {
  it('switches to mini-card mode strictly below 0.6 zoom', () => {
    expect(isMiniCardZoom(0.59)).toBe(true);
    expect(isMiniCardZoom(0.6)).toBe(false);
    expect(isMiniCardZoom(1)).toBe(false);
  });

  it('prioritizes running > error > needsConfiguration in status resolution', () => {
    expect(getNodeCardV2Status({ isPerforming: true, hasRunError: true, hasConfigIssues: true })).toBe('running');
    expect(getNodeCardV2Status({ hasRunError: true, hasConfigIssues: true })).toBe('error');
    expect(getNodeCardV2Status({ hasAdmissionIssues: true })).toBe('needsConfiguration');
    expect(getNodeCardV2Status({})).toBeNull();
  });

  it('derives summaries for core and replatform graph-extension nodes', () => {
    expect(
      getNodeCardV2Summary(
        { id: 'llm-1', type: PipelineNodeTypes.LLM, input_mapping: { task: { type: 'fixed', value: 'Summarize input' } }, tool_names: { github: ['a', 'b'] } },
        { type: PipelineNodeTypes.LLM },
      ),
    ).toEqual({ primary: 'Summarize input', secondary: '1 toolkit · 2 tools' });

    expect(
      getNodeCardV2Summary(
        { id: 'map-1', type: PipelineNodeTypes.Map, source: 'items', worker: 'worker_1' },
        { type: PipelineNodeTypes.Map },
      ),
    ).toEqual({ primary: 'items', secondary: 'Worker → worker_1' });

    expect(
      getNodeCardV2Summary(
        { id: 'par-1', type: PipelineNodeTypes.Parallel, branches: ['b1', 'b2'], join: 'all' },
        { type: PipelineNodeTypes.Parallel },
      ),
    ).toEqual({ primary: '2 branches', secondary: 'all' });

    expect(
      getNodeCardV2Summary(
        { id: 'split-1', type: PipelineNodeTypes.SplitOut, source: 'rows', destination: 'row' },
        { type: PipelineNodeTypes.SplitOut },
      ),
    ).toEqual({ primary: 'rows', secondary: 'row' });

    expect(
      getNodeCardV2Summary(
        { id: 'agg-1', type: PipelineNodeTypes.Aggregate, source: 'rows', operations: [{ op: 'count' }] },
        { type: PipelineNodeTypes.Aggregate },
      ),
    ).toEqual({ primary: 'rows', secondary: '1 operation' });

    expect(
      getNodeCardV2Summary(
        { id: 'par-2', type: PipelineNodeTypes.Parallel, branches: ['b1', 'b2'], output: ['results'] },
        { type: PipelineNodeTypes.Parallel },
      ),
    ).toEqual({ primary: '2 branches', secondary: 'results' });

    expect(
      getNodeCardV2Summary(
        { id: 'split-2', type: PipelineNodeTypes.SplitOut, source: 'rows', output: ['expanded'] },
        { type: PipelineNodeTypes.SplitOut },
      ),
    ).toEqual({ primary: 'rows', secondary: 'expanded' });
  });

  it('uses compact card heights for graph-extension nodes instead of obsolete 750px inline-form heights', () => {
    expect(NodeHeightMap[PipelineNodeTypes.Parallel]).toBeLessThanOrEqual(200);
    expect(NodeHeightMap[PipelineNodeTypes.Map]).toBeLessThanOrEqual(200);
    expect(NodeHeightMap[PipelineNodeTypes.SplitOut]).toBeLessThanOrEqual(200);
    expect(NodeHeightMap[PipelineNodeTypes.Aggregate]).toBeLessThanOrEqual(200);
  });
});

describe('NodeCardView', () => {
  it('renders header, summary lines, admission badge slot, normal handles, and error_output handle slot', () => {
    renderWithTheme(
      <NodeCardView
        id="LLM_1"
        name="Planner LLM"
        type={PipelineNodeTypes.LLM}
        isEntrypoint
        summary={{ primary: 'Plan steps', secondary: '1 toolkit · 2 tools' }}
        admissionBadge={<span data-testid="admission-badge">Admitted</span>}
        handles={(expanded) => <div data-testid="normal-handle">{expanded ? 'expanded' : 'mini'}</div>}
        errorOutputHandle={<div data-testid="error-output-handle">Error output</div>}
      />,
    );

    expect(screen.getByTestId('pipeline-node-v2-card-llm-1')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-node-v2-name')).toHaveTextContent('Planner LLM');
    expect(screen.getByTestId('pipeline-node-v2-type')).toHaveTextContent('LLM');
    expect(screen.getByTestId('pipeline-node-v2-entrypoint')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-node-v2-summary-primary')).toHaveTextContent('Plan steps');
    expect(screen.getByTestId('pipeline-node-v2-summary-secondary')).toHaveTextContent('1 toolkit · 2 tools');
    expect(screen.getByTestId('admission-badge')).toBeInTheDocument();
    expect(screen.getByTestId('normal-handle')).toHaveTextContent('expanded');
    expect(screen.getByTestId('error-output-handle')).toBeInTheDocument();
  });

  it('collapses summary in mini-card mode when zoom < 0.6', () => {
    renderWithTheme(
      <NodeCardView
        id="Agent_1"
        name="Researcher"
        type={PipelineNodeTypes.Agent}
        zoom={0.5}
        summary={{ primary: 'Search docs', secondary: '' }}
        handles={(expanded) => <div data-testid="normal-handle">{expanded ? 'expanded' : 'mini'}</div>}
      />,
    );

    const card = screen.getByTestId('pipeline-node-v2-card-agent-1');
    expect(card).toHaveAttribute('data-mini', 'true');
    expect(screen.queryByTestId('pipeline-node-v2-summary')).not.toBeInTheDocument();
    expect(screen.getByTestId('normal-handle')).toHaveTextContent('mini');
  });

  it('shows needs-configuration status icon and warning summary when issues are present', () => {
    renderWithTheme(
      <NodeCardView
        id="Toolkit_1"
        name="GitHub"
        type={PipelineNodeTypes.Toolkit}
        issues={['Toolkit is required']}
      />,
    );

    expect(screen.getByTestId('pipeline-node-v2-status-needs-configuration')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-node-v2-not-configured')).toHaveTextContent('Not configured');
  });
});
