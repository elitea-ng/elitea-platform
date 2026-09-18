import userEvent from '@testing-library/user-event';
import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { cleanup, waitFor, type RenderResult } from '@testing-library/react';

import { ReactFlow, ReactFlowProvider, type Edge } from '@xyflow/react';

import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { CodeNode } from './CodeNode';

const PROJECT_ID = 'proj-1';

beforeAll(() => {
  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class ResizeObserverStub {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    };
  }
});

afterEach(() => {
  cleanup();
});

function renderCodeNode(flowEditorOverrides: Partial<FlowEditorContextValue> = {}, edges: Edge[] = []): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? { nodes: [{ id: 'Node1' }] };
  const flowEditorValue = buildFlowEditorContextValue({ ...flowEditorOverrides, yamlJsonObject });

  return renderWithRouterAndProject(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
          edges={edges}
          nodeTypes={{ testNode: CodeNode }}
        />
      </FlowEditorContext.Provider>
    </ReactFlowProvider>,
    PROJECT_ID,
  );
}

/** See `PrinterNode.test.tsx`'s identical helper doc comment for the full rationale (out-of-scope `SimpleLLMInputItem` "Type" select drag bug). */
function renderCodeNodeBare(flowEditorOverrides: Partial<FlowEditorContextValue> = {}): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? { nodes: [{ id: 'Node1' }] };
  const flowEditorValue = buildFlowEditorContextValue({ ...flowEditorOverrides, yamlJsonObject });

  return renderWithRouterAndProject(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <CodeNode id="Node1" />
      </FlowEditorContext.Provider>
    </ReactFlowProvider>,
    PROJECT_ID,
  );
}

describe('CodeNode', () => {
  it('renders the node id and both handles (target + source)', async () => {
    const { findByText, container } = renderCodeNode();
    await findByText('Node1');

    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(2);
  });

  it('makes the source handle non-connectable once an outgoing edge to a non-END node already exists', async () => {
    const { findByText, container } = renderCodeNode(
      { yamlJsonObject: { nodes: [{ id: 'Node1' }, { id: 'Other' }] } },
      [{ id: 'e1', source: 'Node1', target: 'Other' }],
    );
    await findByText('Node1');

    const sourceHandle = container.querySelector('[data-handleid="source"]');
    expect(sourceHandle).not.toBeNull();
    expect(sourceHandle?.className).not.toMatch(/\bconnectable\b/);
  });

  it('keeps the source handle connectable when the only outgoing edge targets END', async () => {
    const { findByText, container } = renderCodeNode(
      { yamlJsonObject: { nodes: [{ id: 'Node1' }] } },
      [{ id: 'e1', source: 'Node1', target: 'END' }],
    );
    await findByText('Node1');

    const sourceHandle = container.querySelector('[data-handleid="source"]');
    expect(sourceHandle?.className).toMatch(/\bconnectable\b/);
  });

  it('renders the Code input-mapping row, Input select, Output select, and interrupt settings', async () => {
    const { findByText, getByText } = renderCodeNode();
    await findByText('Node1');

    expect(getByText('Code')).toBeInTheDocument();
    expect(getByText('Input')).toBeInTheDocument();
    expect(getByText('Output')).toBeInTheDocument();
    expect(getByText('Interrupt before')).toBeInTheDocument();
    expect(getByText('Interrupt after')).toBeInTheDocument();
  });

  /* elitea_issues: #2665 — the Code node must never seed a placeholder value ("# Write your code here\"Hello, World!\""); a fresh node's code value is a real empty string. */
  it('defaults the code value to an empty fixed string when yamlNode.code is unset', async () => {
    const { findByText, getAllByText } = renderCodeNode();
    await findByText('Node1');

    expect(getAllByText('Fixed').length).toBeGreaterThan(0);
  });

  it('renders an existing code value from the matching yaml node', async () => {
    const { findByText, getByDisplayValue } = renderCodeNode({
      yamlJsonObject: { nodes: [{ id: 'Node1', code: { type: 'fixed', value: 'print("hi")' } }] },
    });
    await findByText('Node1');

    expect(getByDisplayValue('print("hi")')).toBeInTheDocument();
  });

  it('changing the code mapping type to Variable writes the new type and resets the value', async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn();
    const { findByText, getAllByRole, getByRole } = renderCodeNodeBare({
      yamlJsonObject: {
        nodes: [{ id: 'Node1', code: { type: 'fixed', value: 'old code' } }],
        state: { input: { type: 'str' } },
      },
      setYamlJsonObject,
    });
    await findByText('Code');

    const typeSelect = getAllByRole('combobox', { hidden: true })[0] as HTMLElement;
    await user.click(typeSelect);
    await user.click(getByRole('option', { name: 'Variable' }));

    expect(setYamlJsonObject).toHaveBeenCalledWith(
      expect.objectContaining({
        nodes: [expect.objectContaining({ id: 'Node1', code: { type: 'variable', value: '' } })],
      }),
    );
  });

  it('shows structured output when the CommonInterruptSettings default is used (showStructuredOutput not passed)', async () => {
    const { findByText, getByText } = renderCodeNode();
    await findByText('Node1');

    expect(getByText('Structured output')).toBeInTheDocument();
  });

  it('disables every field while the pipeline is running', async () => {
    const { findByText, getAllByRole } = renderCodeNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }] },
      isRunningPipeline: true,
    });
    await findByText('Node1');

    for (const combobox of getAllByRole('combobox', { hidden: true })) {
      expect(combobox).toHaveAttribute('aria-disabled', 'true');
    }
    // Accessible-name-by-label lookups don't resolve here -- React Flow's
    // inline `visibility: hidden` (never cleared, this env's `ResizeObserver`
    // stub never calls back) makes the accessible-name algorithm treat the
    // associated label text as unavailable, even though `hidden: true`
    // still lets `getAllByRole` find the switch element itself. Asserting
    // "every switch is disabled" (interrupt before/after) is the meaningful,
    // name-independent check available under this harness.
    for (const switchControl of getAllByRole('switch', { hidden: true })) {
      expect(switchControl).toBeDisabled();
    }
  });

  it('disables every field when disabled is set (isRunningPipeline falsy)', async () => {
    const { findByText, getAllByRole } = renderCodeNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }] },
      disabled: true,
    });
    await findByText('Node1');

    for (const switchControl of getAllByRole('switch', { hidden: true })) {
      expect(switchControl).toBeDisabled();
    }
  });

  it('does not throw with no FlowEditorContext ancestor (NodeCard renders null)', async () => {
    const { container } = renderWithRouterAndProject(
      <ReactFlowProvider>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
          edges={[]}
          nodeTypes={{ testNode: CodeNode }}
        />
      </ReactFlowProvider>,
      PROJECT_ID,
    );
    await waitFor(() => expect(container.querySelector('.react-flow')).toBeInTheDocument());

    expect(container.querySelector('.react-flow__handle')).not.toBeInTheDocument();
  });

  /* elitea_issues: #5203 — product gap: no `debug` toggle exists on the Code node, so there is no way to capture the assembled preamble+user code the sandbox actually ran to a `code_debug` artifact. */
  it.fails('elitea_issues 5203: a Debug toggle exists on the Code node for artifact capture', () => {
    const { getByRole } = renderCodeNode();
    expect(getByRole('checkbox', { name: /debug/i })).toBeInTheDocument();
  });
});
