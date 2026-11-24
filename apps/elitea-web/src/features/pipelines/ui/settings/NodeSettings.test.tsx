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
    const { queryByRole } = renderSettings(undefined, 'agent');
    expect(queryByRole('button', { name: 'Settings' })).not.toBeInTheDocument();
  });

  it.each(['llm', 'toolkit', 'mcp'])('shows Retry on fail and Timeout on %s nodes (5b-2 / 5b-3)', async (nodeType) => {
    const { getByRole, getByLabelText } = renderSettings(undefined, nodeType);
    await userEvent.click(getByRole('button', { name: 'Settings' }));
    expect(getByRole('switch', { name: 'Retry on fail' })).toBeEnabled();
    expect(getByLabelText('Timeout (ms)')).toBeEnabled();
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

describe('NodeSettings shows the stored values (browser W1)', () => {
  it.each([
    ['error_output without a target', { on_error: 'error_output' }, 'Continue (using error output)'],
    ['continue', { on_error: 'continue' }, 'Continue'],
    ['an invalid block', { on_error: 'continue', error_transition: 'x' }, 'Continue'],
  ])('displays On error for %s', async (_name, settings, label) => {
    const { getByRole } = renderSettings(settings);
    await userEvent.click(getByRole('button', { name: 'Settings' }));
    expect(getByRole('combobox')).toHaveTextContent(label);
  });

  it('shows the stored toggles even while the block is incomplete', async () => {
    const { getByRole } = renderSettings({ on_error: 'error_output', execute_once: true, always_output_data: true });
    await userEvent.click(getByRole('button', { name: 'Settings' }));
    expect(getByRole('switch', { name: 'Run once per execution' })).toBeChecked();
    expect(getByRole('switch', { name: 'Always output data' })).toBeChecked();
  });
});

describe('NodeSettings Code rows (5b-1 PR B)', () => {
  it('hides Retry on fail and Timeout on deterministic nodes', async () => {
    const { getByRole, queryByRole, queryByLabelText } = renderSettings(undefined, 'state_modifier');
    await userEvent.click(getByRole('button', { name: 'Settings' }));
    expect(queryByRole('switch', { name: 'Retry on fail' })).not.toBeInTheDocument();
    expect(queryByLabelText('Timeout (ms)')).not.toBeInTheDocument();
  });

  it('shows Retry on fail and Timeout on Code, reveals retry fields when enabled, and cleans up defaults', async () => {
    const user = userEvent.setup();
    const first = renderSettings(undefined, 'code');
    await user.click(first.getByRole('button', { name: 'Settings' }));
    expect(first.getByRole('switch', { name: 'Retry on fail' })).not.toBeChecked();
    expect(first.queryByLabelText('Max tries')).not.toBeInTheDocument();
    expect(first.queryByLabelText('Wait between tries (ms)')).not.toBeInTheDocument();
    expect(first.getByLabelText('Timeout (ms)')).toHaveValue(null);

    await user.click(first.getByRole('switch', { name: 'Retry on fail' }));
    expect(first.written()['settings']).toEqual({ retry_on_fail: true });
    first.unmount();

    const second = renderSettings({ retry_on_fail: true, max_tries: 4, wait_between_tries_ms: 500, timeout_ms: 5000 }, 'code');
    await user.click(second.getByRole('button', { name: 'Settings' }));
    expect(second.getByRole('switch', { name: 'Retry on fail' })).toBeChecked();
    expect(second.getByLabelText('Max tries')).toHaveValue(4);
    expect(second.getByLabelText('Wait between tries (ms)')).toHaveValue(500);
    expect(second.getByLabelText('Timeout (ms)')).toHaveValue(5000);

    // Turning Retry on fail off strips max_tries and wait_between_tries_ms while keeping timeout_ms.
    await user.click(second.getByRole('switch', { name: 'Retry on fail' }));
    expect(second.written()['settings']).toEqual({ timeout_ms: 5000 });
    second.unmount();

    // Clearing Timeout when it is the sole setting drops the settings block completely.
    const third = renderSettings({ timeout_ms: 5000 }, 'code');
    await user.click(third.getByRole('button', { name: 'Settings' }));
    await user.clear(third.getByLabelText('Timeout (ms)'));
    expect(Object.hasOwn(third.written(), 'settings')).toBe(false);
  });
});
