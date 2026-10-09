import { cleanup, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

afterEach(() => { cleanup(); vi.unstubAllEnvs(); vi.resetModules(); });
async function load(flag: string | undefined) {
  vi.resetModules();
  vi.stubEnv('VITE_GRAPH_EXTENSIONS_REHEARSAL', flag);
  const contract = await import('../lib/flow-editor/constants/runtimeContract.constants');
  const admission = await import('../lib/graphAdmission.helpers');
  const menu = await import('./AddNodeMenu');
  const parallel = await import('../lib/flow-editor/constants/parallel.constants');
  return { contract, admission, menu, parallel };
}
const document = {
  entry_point: 'split', state: { records: 'list', expanded: 'list' },
  nodes: [{ id: 'split', type: 'split_out', source: 'records', split: { mode: 'list' }, destination: 'item', output: ['expanded'], transition: 'END' }],
};
async function menuEntries(menu: Awaited<ReturnType<typeof load>>['menu']): Promise<readonly (string | null)[]> {
  const user = userEvent.setup();
  renderWithTheme(<menu.AddNodeMenu onAddNode={vi.fn()} />);
  await user.click(screen.getByRole('button', { name: 'Add node' }));
  return within(await screen.findByRole('menu')).getAllByRole('menuitem').map((item) => item.textContent);
}
describe.each([['absent', undefined], ['false', 'false'], ['1', '1']])('graph extensions rehearsal flag %s', (_name, flag) => {
  it('keeps SplitOut and Aggregate hidden and refused', async () => {
    const { contract, admission, menu, parallel } = await load(flag);
    expect(contract.isCompilerAdmittedNodeType('split_out')).toBe(false);
    expect(contract.isCompilerAdmittedNodeType('aggregate')).toBe(false);
    const typeIssues = admission.collectGraphAdmissionIssues(document).filter((issue) => issue.rule === 'node.type');
    expect(typeIssues).toHaveLength(1);
    // Named as a deployment limit, the same way Main's save check names it.
    expect(typeIssues[0]?.message).toBe('type: "split_out" is not available on this deployment — the whole pipeline is refused.');
    expect(contract.isDeploymentGatedNodeType('split_out')).toBe(true);
    expect(contract.isDeploymentGatedNodeType('custom')).toBe(false);
    const entries = await menuEntries(menu);
    expect(entries).toHaveLength(10);
    expect(entries).not.toContain('SplitOut');
    expect(parallel.FIXED_PARALLEL_AUTHORING_ENABLED).toBe(false);
  });
});
describe('graph extensions rehearsal flag true', () => {
  it('admits, shows and stops refusing SplitOut and Aggregate only', async () => {
    const { contract, admission, menu, parallel } = await load('true');
    expect(contract.isCompilerAdmittedNodeType('split_out')).toBe(true);
    expect(contract.isCompilerAdmittedNodeType('aggregate')).toBe(true);
    expect(contract.isCompilerAdmittedNodeType('map')).toBe(false);
    expect(contract.isDeploymentGatedNodeType('split_out')).toBe(false);
    expect(admission.collectGraphAdmissionIssues(document).filter((issue) => issue.rule === 'node.type')).toEqual([]);
    const entries = await menuEntries(menu);
    expect(entries).toHaveLength(12);
    expect(entries).toContain('SplitOut');
    expect(entries).toContain('Aggregate');
    expect(entries).not.toContain('Parallel');
    expect(parallel.FIXED_PARALLEL_AUTHORING_ENABLED).toBe(false);
  });
});
