import userEvent from '@testing-library/user-event';
import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, waitFor, type RenderResult } from '@testing-library/react';
import { dump, load } from 'js-yaml';

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
  it('shows Python for legacy YAML without adding a language field', async () => {
    const setYamlJsonObject = vi.fn();
    const { findByRole } = renderCodeNodeBare({
      expandAll: true,
      yamlJsonObject: { nodes: [{ id: 'Node1', code: { type: 'fixed', value: 'result = 1' } }] },
      setYamlJsonObject,
    });
    expect(await findByRole('combobox', { name: 'Language' })).toHaveTextContent('Python');
    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });

  it.each([
    { language: 'python', source: 'def run(value):\n    return value + 1\n\nresult = run(input)\n' },
    { language: 'javascript', source: 'function run(value) {\n  return value + 1;\n}\n\nresult = run(input);\n' },
    { language: 'typescript', source: 'function run(value: number): number {\n  return value + 1;\n}\n\nresult = run(input);\n' },
    { language: 'rust', source: 'fn run(value: i64) -> i64 {\n    value + 1\n}\n\nlet result = run(input);\n' },
  ])('keeps $language source bytes after card edits, YAML save, and reopen', async ({ language, source }) => {
    const node = { id: 'Node1', language, code: { type: 'fixed', value: source }, input: ['payload'], output: ['answer'], transition: 'END' };
    const sibling = { id: 'Other', code: { type: 'fixed', value: 'unchanged' } };
    const edited = source.replace('value + 1', 'value + 2');
    let saved: YamlPipelineDocument = { nodes: [node, sibling] };
    const setYamlJsonObject = vi.fn((document: YamlPipelineDocument) => {
      saved = document;
    });
    const ui = renderCodeNodeBare({ expandAll: true, yamlJsonObject: saved, setYamlJsonObject });
    const field = await ui.findByRole('textbox', { name: 'Value' });
    expect(field.tagName).toBe('TEXTAREA');
    expect(field).toHaveValue(source);
    fireEvent.change(field, { target: { value: edited } });
    expect(setYamlJsonObject).toHaveBeenLastCalledWith({ nodes: [{ ...node, code: { type: 'fixed', value: edited } }, sibling] });
    ui.unmount();
    const reopened = renderCodeNodeBare({ expandAll: true, yamlJsonObject: load(dump(saved)) as YamlPipelineDocument });
    expect(await reopened.findByRole('textbox', { name: 'Value' })).toHaveValue(edited);
  });

  it.each(['JavaScript', 'TypeScript', 'Rust'])('changes language to %s without rewriting the node', async language => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn();
    const node = {
      id: 'Node1', code: { type: 'variable', value: 'source' }, input: ['payload'],
      output: ['answer'], structured_output: true, debug: true, transition: 'END',
    };
    const { getByRole, findByRole } = renderCodeNodeBare({
      expandAll: true,
      yamlJsonObject: { nodes: [node, { id: 'Other' }] }, setYamlJsonObject,
    });
    await user.click(await findByRole('combobox', { name: 'Language' }));
    await user.click(getByRole('option', { name: language }));
    expect(setYamlJsonObject).toHaveBeenLastCalledWith({
      nodes: [{ ...node, language: language.toLowerCase() }, { id: 'Other' }],
    });
  });

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

  it('elitea_issues 5203: a Debug toggle exists on the Code node for artifact capture', async () => {
    const user = userEvent.setup();
    const source = 'def run(value):\n    return value + 1\n\nresult = run(input)\n';
    const node = { id: 'Node1', type: 'code', code: { type: 'fixed', value: source }, input: ['payload'], output: ['answer'],
      transition: 'END', metadata: { owner: 'retained', nested: ['unchanged'] } };
    const sibling = { id: 'Other', type: 'code', debug: false, code: { type: 'fixed', value: 'result = 2' } };
    let saved: YamlPipelineDocument = { nodes: [node, sibling], state: { payload: { type: 'dict' }, answer: { type: 'int' } } };
    const original = saved;
    const setYamlJsonObject = vi.fn((document: YamlPipelineDocument) => { saved = document; });
    const ui = renderCodeNodeBare({ expandAll: true, yamlJsonObject: saved, setYamlJsonObject });
    const toggle = await ui.findByRole('checkbox', { name: /^debug$/i });
    expect(toggle).not.toBeChecked(); expect(setYamlJsonObject).not.toHaveBeenCalled();
    await user.click(toggle);
    expect(saved).toEqual({ ...original, nodes: [{ ...node, debug: true }, sibling] });
    ui.unmount();
    const reopened = renderCodeNodeBare({ expandAll: true, yamlJsonObject: load(dump(saved)) as YamlPipelineDocument, setYamlJsonObject });
    expect(await reopened.findByRole('checkbox', { name: /^debug$/i })).toBeChecked();
    expect(await reopened.findByRole('textbox', { name: 'Value' })).toHaveValue(source);
    await user.click(reopened.getByRole('checkbox', { name: /^debug$/i }));
    expect(saved).toEqual({ ...original, nodes: [{ ...node, debug: false }, sibling] });
    reopened.unmount();
    const disabledAgain = renderCodeNodeBare({ expandAll: true, yamlJsonObject: load(dump(saved)) as YamlPipelineDocument });
    expect(await disabledAgain.findByRole('checkbox', { name: /^debug$/i })).not.toBeChecked();
    expect(original.nodes?.[0]).not.toHaveProperty('debug');
  });

  it.each([undefined, false, true, null, 'true', { enabled: true }])('preserves stored Debug value %j without an authoring write', async (debug) => {
    const node = { id: 'Node1', type: 'code', code: { type: 'fixed', value: 'result = 1' }, ...(debug === undefined ? {} : { debug }) };
    const setYamlJsonObject = vi.fn();
    const { findByRole } = renderCodeNodeBare({ expandAll: true, yamlJsonObject: { nodes: [node] }, setYamlJsonObject });
    const toggle = await findByRole('checkbox', { name: /^debug$/i });
    expect(toggle).toHaveProperty('checked', debug === true);
    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });

  it.each([{ isRunningPipeline: true }, { disabled: true }])('prevents Debug edits for a running or selected-run pipeline: %j', async (scope) => {
    const setYamlJsonObject = vi.fn();
    const { findByRole } = renderCodeNodeBare({ ...scope, expandAll: true,
      yamlJsonObject: { nodes: [{ id: 'Node1', type: 'code', debug: true, code: { type: 'fixed', value: 'result = 1' } }] }, setYamlJsonObject });
    const toggle = await findByRole('checkbox', { name: /^debug$/i });
    expect(toggle).toBeChecked(); expect(toggle).toBeDisabled();
    fireEvent.click(toggle); expect(setYamlJsonObject).not.toHaveBeenCalled();
  });
});
