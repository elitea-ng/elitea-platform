import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { PipelineNodeTypes } from '../../lib/flow-editor/constants/flowEditor.constants';
import { NODE_FIELD_KEYS } from '../../lib/flow-editor/constants/nodeFieldInfo.constants';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import {
  NodeConfigDrawer,
  NodeConfigPanel,
  NodeFieldHeading,
} from './index';

function createFlowContext(setYamlJsonObject = vi.fn()): FlowEditorContextValue {
  return {
    yamlJsonObject: {
      entry_point: 'LLM_1',
      interrupt_before: [],
      interrupt_after: [],
      nodes: [{ id: 'LLM_1', type: PipelineNodeTypes.LLM }],
    },
    setYamlJsonObject,
    setFlowNodes: vi.fn(),
    setFlowEdges: vi.fn(),
    isRunningPipeline: false,
    disabled: false,
    expandAll: true,
  };
}

describe('NodeConfigDrawer & NodeConfigPanel', () => {
  it('mounts 400px drawer when open, renders header docs link safely, and keeps Q2 Build with AI disabled', () => {
    renderWithTheme(
      <FlowEditorContext.Provider value={createFlowContext()}>
        <NodeConfigDrawer open>
          <NodeConfigPanel
            id="LLM_1"
            type={PipelineNodeTypes.LLM}
            isEntrypoint
            yaml={{ code: 'id: LLM_1\ntype: llm', onChangeCode: vi.fn() }}
            actions={{ onClose: vi.fn(), onDuplicate: vi.fn(), onDelete: vi.fn() }}
            slots={{
              basic: <NodeFieldHeading label="Task" nodeType={PipelineNodeTypes.LLM} fieldKey={NODE_FIELD_KEYS.task} />,
              toolPicker: <div data-testid="tool-picker-slot">Tool picker</div>,
            }}
          />
        </NodeConfigDrawer>
      </FlowEditorContext.Provider>,
    );

    const drawer = screen.getByTestId('pipeline-node-panel-drawer');
    expect(getComputedStyle(drawer).width).toBe('400px');

    const docsLink = screen.getByTestId('pipeline-node-panel-docs');
    expect(docsLink).toHaveAttribute('target', '_blank');
    expect(docsLink).toHaveAttribute('rel', 'noopener noreferrer');

    // Q2: Build with AI stays disabled
    expect(screen.getByTestId('pipeline-node-panel-build-with-ai')).toBeDisabled();
    expect(screen.getByTestId('pipeline-node-panel-entrypoint-state')).toHaveTextContent('Entrypoint');
    expect(screen.getByTestId('tool-picker-slot')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-node-field-info')).toBeInTheDocument();
  });

  it('toggles between Flow view and NodeYamlView and surfaces YAML errors', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <FlowEditorContext.Provider value={createFlowContext()}>
        <NodeConfigPanel
          id="LLM_1"
          type={PipelineNodeTypes.LLM}
          yaml={{ code: 'id: LLM_1', onChangeCode: vi.fn(), error: 'Invalid YAML syntax' }}
          actions={{ onClose: vi.fn() }}
          slots={{ basic: <div data-testid="basic-fields">Fields</div> }}
        />
      </FlowEditorContext.Provider>,
    );

    expect(screen.getByTestId('basic-fields')).toBeInTheDocument();
    const toggleButtons = screen.getByTestId('pipeline-node-panel-view-toggle').querySelectorAll('button');
    await user.click(toggleButtons[1] as HTMLButtonElement);

    expect(screen.getByTestId('pipeline-node-panel-yaml')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-node-panel-yaml-error')).toHaveTextContent('Invalid YAML syntax');
  });

  it('hosts Q6 interrupt switches and NodeSettings inside the Advanced accordion', async () => {
    const user = userEvent.setup();
    const routerContext: FlowEditorContextValue = {
      ...createFlowContext(),
      yamlJsonObject: {
        entry_point: 'Router_1',
        interrupt_before: [],
        interrupt_after: [],
        nodes: [{ id: 'Router_1', type: PipelineNodeTypes.Router }],
      },
    };
    renderWithTheme(
      <FlowEditorContext.Provider value={routerContext}>
        <NodeConfigPanel
          id="Router_1"
          type={PipelineNodeTypes.Router}
          yaml={{ code: 'id: Router_1', onChangeCode: vi.fn() }}
          actions={{ onClose: vi.fn() }}
          slots={{ basic: <div data-testid="basic-fields">Fields</div> }}
        />
      </FlowEditorContext.Provider>,
    );

    const advancedSection = screen.getByTestId('pipeline-node-panel-advanced-section');
    await user.click(advancedSection.querySelector('button') as HTMLButtonElement);

    expect(screen.getByText('Interrupt before')).toBeInTheDocument();
    expect(screen.getByText('Interrupt after')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Settings' })).toBeInTheDocument();
  });

  it('invokes Make Entrypoint, Duplicate, Delete, and Close callbacks', async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    const onMakeEntrypoint = vi.fn();
    const onDuplicate = vi.fn();
    const onDelete = vi.fn();

    renderWithTheme(
      <FlowEditorContext.Provider value={createFlowContext()}>
        <NodeConfigPanel
          id="LLM_1"
          type={PipelineNodeTypes.LLM}
          isEntrypoint={false}
          yaml={{ code: 'id: LLM_1', onChangeCode: vi.fn() }}
          actions={{ onClose, onMakeEntrypoint, onDuplicate, onDelete }}
        />
      </FlowEditorContext.Provider>,
    );

    await user.click(screen.getByTestId('pipeline-node-panel-make-entrypoint'));
    await user.click(screen.getByTestId('pipeline-node-panel-duplicate'));
    await user.click(screen.getByTestId('pipeline-node-panel-delete'));
    await user.click(screen.getByTestId('pipeline-node-panel-close'));

    expect(onMakeEntrypoint).toHaveBeenCalledWith('LLM_1');
    expect(onDuplicate).toHaveBeenCalledWith('LLM_1');
    expect(onDelete).toHaveBeenCalledWith('LLM_1');
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
