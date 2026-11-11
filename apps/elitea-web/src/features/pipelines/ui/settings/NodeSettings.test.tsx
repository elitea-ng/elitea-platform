import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { buildFlowEditorContextValue } from '../../__tests__/testUtils';
import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { ERROR_OUTPUT_HANDLE } from '../../lib/flow-editor/helpers/nodeErrorRoute.helpers';
import type { FlowEdge, SetFlowEdges } from '../../lib/flow-editor/reactFlowTypes';
import { NodeSettings } from './NodeSettings';

function renderSettings(settings: Record<string, unknown> | undefined, type = 'state_modifier', setFlowEdges = vi.fn()) {
  const document: YamlPipelineDocument = {
    entry_point: 'a',
    nodes: [{ id: 'a', type, template: 'x', transition: 'END', ...(settings ? { settings } : {}) }],
  };
  const setYamlJsonObject = vi.fn();
  const rendered = renderWithTheme(
    <FlowEditorContext.Provider value={buildFlowEditorContextValue({ yamlJsonObject: document, setYamlJsonObject, setFlowEdges })}>
      <NodeSettings id="a" type={type} disabled={false} />
    </FlowEditorContext.Provider>,
  );
  const written = (): Record<string, unknown> => {
    const next = setYamlJsonObject.mock.calls.at(-1)?.[0] as YamlPipelineDocument;
    return next.nodes?.[0] as unknown as Record<string, unknown>;
  };
  return { ...rendered, setYamlJsonObject, written };
}

describe('NodeSettings', () => {
  it('is collapsed until opened, then shows the three settings', async () => {
    const { getByRole, queryByRole } = renderSettings(undefined);
    expect(queryByRole('switch', { name: 'Run once per execution' })).not.toBeInTheDocument();
    await userEvent.click(getByRole('button', { name: 'Settings' }));
    expect(getByRole('switch', { name: 'Run once per execution' })).toBeEnabled();
    expect(getByRole('switch', { name: 'Always output data' })).toBeEnabled();
    expect(getByRole('combobox')).toBeInTheDocument();
  });

  it('writes a setting and drops the block when it returns to its default', async () => {
    const first = renderSettings(undefined);
    await userEvent.click(first.getByRole('button', { name: 'Settings' }));
    await userEvent.click(first.getByRole('switch', { name: 'Run once per execution' }));
    expect(first.written()['settings']).toEqual({ execute_once: true });
    first.unmount();

    const second = renderSettings({ execute_once: true });
    await userEvent.click(second.getByRole('button', { name: 'Settings' }));
    await userEvent.click(second.getByRole('switch', { name: 'Run once per execution' }));
    expect(Object.hasOwn(second.written(), 'settings')).toBe(false);
  });

  it('renders nothing for a node type without settings yet', () => {
    const { queryByRole } = renderSettings(undefined, 'llm');
    expect(queryByRole('button', { name: 'Settings' })).not.toBeInTheDocument();
  });
});

describe('NodeSettings error route (fix round 1, finding 9)', () => {
  it('removes the error output edge when On error leaves error output', async () => {
    let edges: FlowEdge[] = [
      { id: 'error', source: 'a', target: 'b', sourceHandle: ERROR_OUTPUT_HANDLE },
      { id: 'normal', source: 'a', target: 'c' },
    ];
    const setFlowEdges = vi.fn<SetFlowEdges>((update) => {
      edges = typeof update === 'function' ? update(edges) : update;
    });
    const { getByRole, findByRole, written } = renderSettings({ on_error: 'error_output', error_transition: 'b' }, 'state_modifier', setFlowEdges);
    await userEvent.click(getByRole('button', { name: 'Settings' }));
    await userEvent.click(getByRole('combobox'));
    await userEvent.click(await findByRole('option', { name: 'Stop pipeline' }));
    expect(Object.hasOwn(written(), 'settings')).toBe(false);
    expect(edges.map((edge) => edge.id)).toEqual(['normal']);
  });
});
