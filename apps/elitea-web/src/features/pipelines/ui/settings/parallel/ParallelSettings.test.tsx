import type { YamlPipelineDocument } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import userEvent from '@testing-library/user-event';
import { cleanup } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { currentExtensionDocument, renderExtensionCard } from '../../../__tests__/graphExtensionTestUtils';
import { EditableParallelSettings, parallelYaml } from '../../../__tests__/parallelTestUtils';
import { parsePipelineYamlDocument } from '../../../lib/pipelineYamlDocument.helpers';
import { readPipelineStateOrder } from '../../../lib/pipelineYamlState.helpers';
import { graphExtensionReferenceUpdate } from '../../../lib/graphExtensionReferenceRename.helpers';
import { usePipelineYamlStore } from '../../../model/pipelineYamlStore';

afterEach(cleanup);
const owner = () => currentExtensionDocument().nodes?.find((node) => node.id === 'extension');
describe('fixed Parallel settings and YAML roundtrip', () => {
  it('renders exact identities, mapping modes, ordered output and policy defaults without deriving YAML', async () => {
    const screen = renderExtensionCard(EditableParallelSettings, parallelYaml);
    expect(await screen.findByLabelText('Branch 1 result key')).toHaveValue('stable_left');
    expect(screen.getByLabelText('Branch 1 Agent node')).toHaveValue('Agent_left');
    expect(screen.getByLabelText('Error policy')).toHaveValue('fail_after_drain');
    expect(screen.getByText(/Task mapping: variable/u)).toBeVisible();
    expect(screen.getByText(/Task mapping: fstring/u)).toBeVisible();
    expect(screen.queryByLabelText('Reduction')).toBeNull();
    expect(screen.queryByLabelText('Child item channel')).toBeNull();
    expect(usePipelineYamlStore.getState().yamlCode).toBe(parallelYaml);
    expect(owner()).not.toHaveProperty('error_policy');
  });
  it('saves concurrency and branch order, then reopens exact mappings, child identity, opaque state, and authored state order', async () => {
    const screen = renderExtensionCard(EditableParallelSettings, parallelYaml);
    const before = currentExtensionDocument();
    const user = userEvent.setup();
    const field = await screen.findByLabelText('Maximum concurrent branches');
    await user.clear(field); await user.type(field, '1');
    await user.click(screen.getByRole('button', { name: 'Move branch 2 up' }));
    const saved = usePipelineYamlStore.getState().yamlCode;
    const reopened = parsePipelineYamlDocument(saved).yamlJsonObject as YamlPipelineDocument;
    expect(reopened.nodes?.[0]?.['max_concurrency']).toBe(1);
    expect(reopened.nodes?.[0]?.['branches']).toEqual([{ id: 'stable_right', node: 'Agent_right' }, { id: 'stable_left', node: 'Agent_left' }]);
    expect(reopened.nodes?.slice(1)).toEqual(before.nodes?.slice(1));
    expect(reopened.state).toEqual(before.state);
    expect(readPipelineStateOrder(saved)).toEqual(['10', '2', 'note', 'prefix']);
    cleanup();
    const again = renderExtensionCard(EditableParallelSettings, saved);
    expect(await again.findByLabelText('Branch 1 result key')).toHaveValue('stable_right');
    expect(again.getByLabelText('Branch 1 Agent node')).toHaveValue('Agent_right');
  });
  it('changes only a selected branch field and preserves invalid foreign branch metadata and null siblings', async () => {
    const yaml = parallelYaml.replace('node: Agent_left}', 'node: Agent_left, foreign: {opaque: [null, true]}}').replace('{id: stable_right, node: Agent_right}', 'null');
    const screen = renderExtensionCard(EditableParallelSettings, yaml);
    const before = currentExtensionDocument();
    const user = userEvent.setup();
    const field = await screen.findByLabelText('Branch 1 result key');
    await user.clear(field); await user.type(field, 'replacement');
    expect(owner()?.['branches']).toEqual([{ id: 'replacement', node: 'Agent_left', foreign: { opaque: [null, true] } }, null]);
    expect(currentExtensionDocument().nodes?.slice(1)).toEqual(before.nodes?.slice(1));
    expect(currentExtensionDocument().state).toEqual(before.state);
  });
  it('adds stable result keys and removes one explicit row without changing existing identities', async () => {
    const screen = renderExtensionCard(EditableParallelSettings, parallelYaml);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Add fixed branch' }));
    expect(owner()?.['branches']).toEqual([{ id: 'stable_left', node: 'Agent_left' }, { id: 'stable_right', node: 'Agent_right' }, { id: 'branch_1', node: '' }]);
    await user.click(screen.getByRole('button', { name: 'Remove branch 3' }));
    expect(owner()?.['branches']).toEqual([{ id: 'stable_left', node: 'Agent_left' }, { id: 'stable_right', node: 'Agent_right' }]);
  });
  it('changes only the declared list destination and never creates or modifies state descriptors', async () => {
    const screen = renderExtensionCard(EditableParallelSettings, parallelYaml);
    const before = currentExtensionDocument();
    const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText('Ordered list output'), '2');
    expect(owner()?.output).toEqual(['2']);
    expect(currentExtensionDocument().state).toEqual(before.state);
    expect(currentExtensionDocument().nodes?.slice(1)).toEqual(before.nodes?.slice(1));
    expect(owner()).not.toHaveProperty('input_mapping');
  });
  it('selects an exact new Agent identity without recreating its task or changing the stable result key', async () => {
    const yaml = parallelYaml + `  - id: Agent_extra
    type: agent
    tool: right_participant
    transition: END
    input_mapping: {task: {type: fixed, value: 'Exact saved task.'}}
    output: [note]
`;
    const screen = renderExtensionCard(EditableParallelSettings, yaml);
    const before = currentExtensionDocument();
    const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText('Branch 2 Agent node'), 'Agent_extra');
    expect(owner()?.['branches']).toEqual([{ id: 'stable_left', node: 'Agent_left' }, { id: 'stable_right', node: 'Agent_extra' }]);
    expect(currentExtensionDocument().nodes?.slice(1)).toEqual(before.nodes?.slice(1));
    expect(screen.getByRole('button', { name: 'Remove transition from Agent_extra' })).toBeEnabled();
    expect(currentExtensionDocument().state).toEqual(before.state);
  });
  it('removes only the uniquely owned Agent transition after an explicit action', async () => {
    const yaml = parallelYaml.replace('    tool: left_participant', '    transition: END\n    tool: left_participant');
    const screen = renderExtensionCard(EditableParallelSettings, yaml);
    const before = currentExtensionDocument();
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Remove transition from Agent_left' }));
    expect(currentExtensionDocument().nodes?.[1]).toEqual(Object.fromEntries(Object.entries(before.nodes?.[1] ?? {}).filter(([key]) => key !== 'transition')));
    expect(owner()).toEqual(before.nodes?.[0]);
    expect(currentExtensionDocument().nodes?.[2]).toEqual(before.nodes?.[2]);
  });
  it('withholds child transition actions when Map or another Parallel shares the exact Agent', async () => {
    const yaml = parallelYaml.replace('    tool: left_participant', '    transition: END\n    tool: left_participant') + '  - {id: foreign_map, type: map, worker: Agent_left}\n';
    const screen = renderExtensionCard(EditableParallelSettings, yaml);
    await screen.findByLabelText('Branch 1 Agent node');
    expect(screen.queryByRole('button', { name: 'Remove transition from Agent_left' })).toBeNull();
    expect(usePipelineYamlStore.getState().yamlCode).toBe(yaml);
  });
  it('keeps invalid authored policies visible and disables every settings action during playback', async () => {
    const yaml = parallelYaml.replace('    wait: all', '    wait: unsupported\n    error_policy: fail_fast');
    const screen = renderExtensionCard(EditableParallelSettings, yaml, true);
    expect(await screen.findByLabelText('Wait policy')).toHaveValue('unsupported');
    expect(screen.getByLabelText('Error policy')).toHaveValue('fail_fast');
    for (const field of screen.getAllByRole('textbox')) expect(field).toBeDisabled();
    for (const field of screen.getAllByRole('combobox')) expect(field).toBeDisabled();
    expect(screen.getByLabelText('Maximum concurrent branches')).toBeDisabled();
    for (const button of screen.getAllByRole('button')) expect(button).toBeDisabled();
    expect(usePipelineYamlStore.getState().yamlCode).toBe(yaml);
  });
  it('uses the existing rename seam for child node references while preserving stable result keys and foreign data', () => {
    const rows = [{ id: 'Agent_left', node: 'Agent_left', foreign: { opaque: null } }, { id: 'right', node: 'Agent_right' }];
    const node = { id: 'extension', type: 'parallel', branches: rows };
    const renamed = graphExtensionReferenceUpdate(node, 'Agent_left', 'Renamed_agent');
    expect(renamed['branches']).toEqual([{ id: 'Agent_left', node: 'Renamed_agent', foreign: { opaque: null } }, rows[1]]);
  });
});
