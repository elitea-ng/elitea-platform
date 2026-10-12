import { cleanup, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { collectGraphAdmissionIssues } from '../lib/graphAdmission.helpers';
import { isCompilerAdmittedNodeType } from '../lib/flow-editor/constants/runtimeContract.constants';
import { FIXED_PARALLEL_AUTHORING_ENABLED } from '../lib/flow-editor/constants/parallel.constants';
import { AddNodeMenu } from './AddNodeMenu';

afterEach(cleanup);

const document = {
  entry_point: 'split', state: { records: 'list', expanded: 'list' },
  nodes: [{ id: 'split', type: 'split_out', source: 'records', split: { mode: 'list' }, destination: 'item', output: ['expanded'], transition: 'END' }],
};

describe('SplitOut and Aggregate are admitted by default', () => {
  it('admits both node types and stops refusing them at save time', () => {
    expect(isCompilerAdmittedNodeType('split_out')).toBe(true);
    expect(isCompilerAdmittedNodeType('aggregate')).toBe(true);
    expect(collectGraphAdmissionIssues(document).filter((issue) => issue.rule === 'node.type')).toEqual([]);
  });

  it('still refuses a type no runtime runs, and the unmerged map type', () => {
    expect(isCompilerAdmittedNodeType('map')).toBe(false);
    const issues = collectGraphAdmissionIssues({ ...document, nodes: [{ id: 'split', type: 'custom', transition: 'END' }] })
      .filter((issue) => issue.rule === 'node.type');
    expect(issues).toHaveLength(1);
    expect(issues[0]?.message).toBe('type: "custom" is not a node type this runtime can run — the whole pipeline is refused.');
  });

  it('offers SplitOut and Aggregate in the Add node menu, and still withholds Parallel', async () => {
    const user = userEvent.setup();
    renderWithTheme(<AddNodeMenu onAddNode={vi.fn()} />);
    await user.click(screen.getByRole('button', { name: 'Add node' }));
    const entries = within(await screen.findByRole('menu')).getAllByRole('menuitem').map((item) => item.textContent);
    expect(entries).toHaveLength(12);
    expect(entries).toContain('SplitOut');
    expect(entries).toContain('Aggregate');
    expect(entries).not.toContain('Parallel');
    expect(FIXED_PARALLEL_AUTHORING_ENABLED).toBe(false);
  });
});
