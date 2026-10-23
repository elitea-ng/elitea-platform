import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import type { Workspace } from '@/shared/desktop/workspaceIpc';

import { folderContextActions } from '../lib/folderMenu';
import { ProjectPickerSheet } from '../ui/ProjectPickerSheet';

const WORKSPACE: Workspace = { id: 'w1', path: '/Users/me/app', name: 'app', project_id: 3, is_git: true };
const projects = (n: number) => Array.from({ length: n }, (_, i) => ({ id: i + 1, name: `Project ${String(i + 1)}`, suspended: i === 0 }));

function build(count: number, choosePicker = vi.fn()) {
  return folderContextActions({
    workspace: WORKSPACE,
    projects: projects(count),
    appIpc: undefined,
    primary: { label: 'Open', run: vi.fn() },
    bind: vi.fn(),
    remove: vi.fn(),
    choosePicker,
  });
}

const submenuOf = (actions: ReturnType<typeof build>) => {
  const sub = actions.find((a) => 'submenu' in a);
  return sub !== undefined && 'submenu' in sub ? sub.submenu : [];
};

describe('folderContextActions', () => {
  it('lists the bindable projects (suspended ones never), marking the bound one', () => {
    const labels = submenuOf(build(4)).map((c) => c.label);
    expect(labels).toEqual(['Project 2', '✓ Project 3', 'Project 4']);
  });

  it('caps the submenu and offers "More projects…", which opens the picker', () => {
    const choosePicker = vi.fn();
    const sub = submenuOf(build(80, choosePicker));
    expect(sub).toHaveLength(21);
    expect(sub[20]?.label).toBe('More projects…');
    sub[20]?.run();
    expect(choosePicker).toHaveBeenCalledTimes(1);
  });

  it('truncates a long project name', () => {
    const [first] = submenuOf(folderContextActions({ workspace: WORKSPACE, projects: [{ id: 9, name: 'x'.repeat(200) }], appIpc: undefined, primary: { label: 'Open', run: vi.fn() }, bind: vi.fn(), remove: vi.fn(), choosePicker: vi.fn() }));
    expect(first?.label.length).toBeLessThanOrEqual(60);
  });
});

describe('ProjectPickerSheet', () => {
  it('lists every bindable project and binds the one picked', async () => {
    const onPick = vi.fn();
    const onClose = vi.fn();
    render(
      <AppProviders>
        <ProjectPickerSheet open projects={projects(40)} currentId={3} onPick={onPick} onClose={onClose} />
      </AppProviders>,
    );
    expect(screen.getAllByRole('menuitemradio')).toHaveLength(39);
    expect(screen.getByRole('menuitemradio', { name: 'Project 3' })).toHaveAttribute('aria-checked', 'true');
    await userEvent.setup().click(screen.getByRole('menuitemradio', { name: 'Project 40' }));
    expect(onPick).toHaveBeenCalledWith(40);
    expect(onClose).toHaveBeenCalled();
  });
});
