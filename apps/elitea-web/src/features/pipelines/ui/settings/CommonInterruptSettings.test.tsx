import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { buildFlowEditorContextValue } from '../../__tests__/testUtils';
import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import type { FlowEdge, SetFlowEdges } from '../../lib/flow-editor/reactFlowTypes';
import { CommonInterruptSettings, type CommonInterruptSettingsProps } from './CommonInterruptSettings';

const DOCUMENT: YamlPipelineDocument = {
  entry_point: 'Tool_0',
  nodes: [
    { id: 'Tool_0', type: 'toolkit', transition: 'Tool_1' },
    { id: 'Tool_1', type: 'toolkit', transition: 'Tool_2' },
    { id: 'Tool_2', type: 'toolkit', transition: 'END' },
  ],
};
const EDGES: FlowEdge[] = [
  { id: 'incoming', source: 'Tool_0', target: 'Tool_1', data: { custom: 'preserved' } },
  { id: 'outgoing', source: 'Tool_1', target: 'Tool_2' },
  { id: 'end', source: 'Tool_2', target: 'END' },
];

function renderSettings(document: YamlPipelineDocument = DOCUMENT, props: Partial<CommonInterruptSettingsProps> = {}) {
  const setYamlJsonObject = vi.fn();
  const setFlowEdges = vi.fn<SetFlowEdges>();
  const contextValue = buildFlowEditorContextValue({ yamlJsonObject: document, setYamlJsonObject, setFlowEdges });
  const rendered = renderWithTheme(
    <FlowEditorContext.Provider value={contextValue}>
      <CommonInterruptSettings id="Tool_1" type="toolkit" {...props} />
    </FlowEditorContext.Provider>,
  );
  return { ...rendered, setYamlJsonObject, setFlowEdges };
}

function updatedEdges(setFlowEdges: ReturnType<typeof vi.fn<SetFlowEdges>>, edges: FlowEdge[] = EDGES): FlowEdge[] {
  const update = setFlowEdges.mock.calls[0]?.[0];
  if (typeof update !== 'function') throw new Error('Expected an edge update function');
  return update(edges);
}

describe('CommonInterruptSettings', () => {
  it('renders the three switches by default', () => {
    const { getByRole } = renderSettings();

    expect(getByRole('switch', { name: 'Interrupt before' })).toBeEnabled();
    expect(getByRole('switch', { name: 'Interrupt after' })).toBeEnabled();
    expect(getByRole('switch', { name: 'Structured output' })).toBeInTheDocument();
  });

  it('hides structured output when requested', () => {
    const { queryByRole } = renderSettings(DOCUMENT, { showStructuredOutput: false });

    expect(queryByRole('switch', { name: 'Structured output' })).not.toBeInTheDocument();
  });

  it.each([
    ['interrupt_before', 'Interrupt before', 'incoming'],
    ['interrupt_after', 'Interrupt after', 'outgoing'],
  ] as const)('authors %s with the exact stored node id and labels its edge', async (field, label, edgeId) => {
    const user = userEvent.setup();
    const document = { ...DOCUMENT, extension: { preserved: true } };
    const { getByRole, queryByTestId, setYamlJsonObject, setFlowEdges } = renderSettings(document);

    await user.click(getByRole('switch', { name: label }));

    expect(setYamlJsonObject).toHaveBeenCalledWith({ ...document, [field]: ['Tool_1'] });
    const edges = updatedEdges(setFlowEdges);
    expect(edges.find((edge) => edge.id === edgeId)?.data?.label).toBe('interrupt');
    expect(edges.find((edge) => edge.id === 'incoming')?.data?.custom).toBe('preserved');
    expect(edges.find((edge) => edge.id === 'end')).toBe(EDGES[2]);
    expect(queryByTestId('interrupt-withheld-reason')).not.toBeInTheDocument();
  });

  it.each([
    ['interrupt_before', 'Interrupt before', 'incoming'],
    ['interrupt_after', 'Interrupt after', 'outgoing'],
  ] as const)('removes only the current node from %s', async (field, label, edgeId) => {
    const user = userEvent.setup();
    const document = { ...DOCUMENT, [field]: ['Tool_0', 'Tool_1', 'Tool_2'] };
    const { getByRole, setYamlJsonObject, setFlowEdges } = renderSettings(document);

    expect(getByRole('switch', { name: label })).toBeChecked();
    await user.click(getByRole('switch', { name: label }));

    expect(setYamlJsonObject).toHaveBeenCalledWith({ ...document, [field]: ['Tool_0', 'Tool_2'] });
    const edges = EDGES.map((edge) => ({ ...edge, data: { ...edge.data, label: 'interrupt' } }));
    expect(updatedEdges(setFlowEdges, edges).find((edge) => edge.id === edgeId)?.data?.label).toBeUndefined();
  });

  it.each([
    ['interrupt_before', 'Interrupt before', 'incoming', 'Tool_0'],
    ['interrupt_after', 'Interrupt after', 'outgoing', 'Tool_2'],
  ] as const)('retains the neighboring pause label when %s is removed', async (field, label, edgeId, neighbor) => {
    const user = userEvent.setup();
    const otherField = field === 'interrupt_before' ? 'interrupt_after' : 'interrupt_before';
    const document = { ...DOCUMENT, [field]: ['Tool_1'], [otherField]: [neighbor] };
    const { getByRole, setYamlJsonObject, setFlowEdges } = renderSettings(document);

    await user.click(getByRole('switch', { name: label }));

    expect(setYamlJsonObject).toHaveBeenCalledWith({ ...document, [field]: [] });
    expect(updatedEdges(setFlowEdges).find((edge) => edge.id === edgeId)?.data?.label).toBe('interrupt');
  });

  it.each([
    ['Tool_0', 'interrupt_before', 'Interrupt before'],
    ['Tool_2', 'interrupt_after', 'Interrupt after'],
  ] as const)('allows a pause at %s, including entry and END-transition nodes', async (id, field, label) => {
    const user = userEvent.setup();
    const { getByRole, setYamlJsonObject, setFlowEdges } = renderSettings(DOCUMENT, { id });
    const control = getByRole('switch', { name: label });

    expect(control).toBeEnabled();
    await user.click(control);

    expect(setYamlJsonObject).toHaveBeenCalledWith({ ...DOCUMENT, [field]: [id] });
    if (field === 'interrupt_after') expect(updatedEdges(setFlowEdges).find((edge) => edge.id === 'end')?.data?.label).toBe('interrupt');
  });

  it('shows stored entry and END-transition pauses as checked', () => {
    const document = { ...DOCUMENT, entry_point: 'Tool_1', nodes: [{ id: 'Tool_1', type: 'toolkit', transition: 'END' }], interrupt_before: ['Tool_1'], interrupt_after: ['Tool_1'] };
    const { getByRole } = renderSettings(document);

    expect(getByRole('switch', { name: 'Interrupt before' })).toBeChecked();
    expect(getByRole('switch', { name: 'Interrupt after' })).toBeChecked();
  });

  it('respects the disabled setting', async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const { getByRole, setYamlJsonObject, setFlowEdges } = renderSettings(DOCUMENT, { disabled: true });

    for (const label of ['Interrupt before', 'Interrupt after', 'Structured output']) {
      const control = getByRole('switch', { name: label });
      expect(control).toBeDisabled();
      await user.click(control);
    }
    expect(setYamlJsonObject).not.toHaveBeenCalled();
    expect(setFlowEdges).not.toHaveBeenCalled();
  });

  it('does not author a pause for a node outside the stored document', () => {
    const { getByRole } = renderSettings(DOCUMENT, { id: 'Missing' });

    expect(getByRole('switch', { name: 'Interrupt before' })).toBeDisabled();
    expect(getByRole('switch', { name: 'Interrupt after' })).toBeDisabled();
  });

  it('updates structured_output on toggle', async () => {
    const user = userEvent.setup();
    const { getByRole, setYamlJsonObject } = renderSettings();

    await user.click(getByRole('switch', { name: 'Structured output' }));

    expect(setYamlJsonObject).toHaveBeenCalledWith(expect.objectContaining({
      nodes: [DOCUMENT.nodes?.[0], expect.objectContaining({ id: 'Tool_1', structured_output: true }), DOCUMENT.nodes?.[2]],
    }));
  });

  it('degrades malformed non-array pause values to empty lists without throwing', () => {
    const document = { ...DOCUMENT, interrupt_before: 'not-an-array', interrupt_after: { also: 'not-an-array' } } as unknown as YamlPipelineDocument;
    const { getByRole, setYamlJsonObject } = renderSettings(document);

    expect(getByRole('switch', { name: 'Interrupt before' })).not.toBeChecked();
    expect(getByRole('switch', { name: 'Interrupt after' })).not.toBeChecked();
    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });

  it('does not throw with no FlowEditorContext ancestor', () => {
    expect(() => renderWithTheme(<CommonInterruptSettings id="Tool_1" type="toolkit" />)).not.toThrow();
  });
});
