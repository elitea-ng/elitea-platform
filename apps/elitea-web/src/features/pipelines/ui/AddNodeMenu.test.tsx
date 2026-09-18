import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { AddNodeMenu } from './AddNodeMenu';
import * as RuntimeContractConstants from '../lib/flow-editor/constants/runtimeContract.constants';

describe('AddNodeMenu', () => {
  it('opens the menu on trigger click and lists every non-deprecated node type once, alphabetically', async () => {
    const user = userEvent.setup();
    renderWithTheme(<AddNodeMenu onAddNode={vi.fn()} />);

    await user.click(screen.getByRole('button', { name: 'Add node' }));

    const menu = await screen.findByRole('menu');
    const items = within(menu).getAllByRole('menuitem');
    const labels = items.map((item) => item.textContent);

    // Deprecated/invisible node types must never appear.
    expect(labels).not.toContain('Tool');
    expect(labels).not.toContain('Function');
    expect(labels).not.toContain('Condition');
    expect(labels).not.toContain('Pipeline');
    expect(labels).not.toContain('Loop');
    expect(labels).not.toContain('Loop from tool');
    expect(labels).not.toContain('End');
    expect(labels).not.toContain('Ghost');
    expect(labels).not.toContain('Default');

    // A representative non-deprecated type must be present.
    expect(labels).toContain('Agent');
    expect(labels).toContain('LLM');

    // Sorted case-insensitively, ascending.
    const sorted = [...labels].sort((a, b) => (a ?? '').toLowerCase().localeCompare((b ?? '').toLowerCase()));
    expect(labels).toEqual(sorted);
  });

  /**
   * The menu offered `Code` and `Custom`, for which the Rust pipeline
   * compiler has no `parse_pipeline_node` arm at all
   * (`services/elitea-worker-rust/src/agents/graph/compiler.rs:1236`), so
   * adding either produced a pipeline that could not load ("the pipeline
   * contains a node type that is not enabled", `compiler.rs:1267`). Both are
   * withheld from AUTHORING; their renderers stay registered so stored
   * documents containing them still display.
   */
  it('offers exactly the nine node types the pipeline compiler admits — no Code, no Custom', async () => {
    const user = userEvent.setup();
    renderWithTheme(<AddNodeMenu onAddNode={vi.fn()} />);

    await user.click(screen.getByRole('button', { name: 'Add node' }));
    const menu = await screen.findByRole('menu');
    const labels = within(menu)
      .getAllByRole('menuitem')
      .map(item => item.textContent);

    expect(labels).not.toContain('Code');
    expect(labels).not.toContain('Custom');

    expect([...labels].sort()).toEqual(
      [
        'Agent',
        'Decision',
        'Human-in-the-loop',
        'LLM',
        'MCP',
        'Printer',
        'Router',
        'State modifier',
        'Toolkit',
      ].sort(),
    );
  });

  /* elitea_issues: #2661 — the node picker must render exactly 2 columns, not 3. */
  it('renders the node list in exactly 2 columns', async () => {
    const user = userEvent.setup();
    renderWithTheme(<AddNodeMenu onAddNode={vi.fn()} />);

    await user.click(screen.getByRole('button', { name: 'Add node' }));
    const menu = await screen.findByRole('menu');
    const columns = within(menu).getAllByTestId('add-node-menu-column');

    expect(columns).toHaveLength(2);
  });

  it.each([
    'Agent',
    'Decision',
    'Human-in-the-loop',
    'LLM',
    'MCP',
    'Printer',
    'Router',
    'State modifier',
    'Toolkit',
  ])('the %s item dispatches a node type the compiler admits, by value not by label', async label => {
    // Guards the wiring, not just the copy: a label can be right while the
    // `onAddNode` payload is a type the runtime refuses.
    const user = userEvent.setup();
    const onAddNode = vi.fn();
    renderWithTheme(<AddNodeMenu onAddNode={onAddNode} />);

    await user.click(screen.getByRole('button', { name: 'Add node' }));
    const menu = await screen.findByRole('menu');
    await user.click(within(menu).getByText(label));

    const type = onAddNode.mock.calls[0]?.[0] as string;
    expect(RuntimeContractConstants.isCompilerAdmittedNodeType(type)).toBe(true);
  });

  it('calls onAddNode with the clicked type and closes the menu', async () => {
    const user = userEvent.setup();
    const onAddNode = vi.fn();
    renderWithTheme(<AddNodeMenu onAddNode={onAddNode} />);

    await user.click(screen.getByRole('button', { name: 'Add node' }));
    const menu = await screen.findByRole('menu');
    await user.click(within(menu).getByText('Agent'));

    expect(onAddNode).toHaveBeenCalledWith('agent');
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  it('disables the trigger button when disabled is true', () => {
    renderWithTheme(<AddNodeMenu onAddNode={vi.fn()} disabled />);
    expect(screen.getByRole('button', { name: 'Add node' })).toBeDisabled();
  });
});
