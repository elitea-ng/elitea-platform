/**
 * The Workspaces list names what each folder is: a Git chip only inside a git work tree, a Code or Documents chip for
 * a folder without git (IPC.md "Workspaces", `kind`), and none for a mixed one.
 */
import { screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { WorkspaceList } from './WorkspaceList';

const folder = (id: string, extra: Partial<Workspace>): Workspace => ({ id, path: `/f/${id}`, name: id, project_id: 1, is_git: false, ...extra });

describe('WorkspaceList folder kind chips', () => {
  it('shows Git, Code or Documents by what the folder holds, and nothing for a mixed folder', () => {
    renderWithTheme(
      <WorkspaceList
        workspaces={[
          folder('repo', { is_git: true, kind: 'git' }),
          folder('tool', { kind: 'code' }),
          folder('papers', { kind: 'documents' }),
          folder('both', { kind: 'mixed' }),
          folder('older-host', {}),
        ]}
        projects={[{ id: 1, name: 'P' }]}
        onSelect={vi.fn()}
        onContextMenu={vi.fn()}
      />,
    );
    const chipOf = (name: string): string | null => {
      const row = screen.getAllByTestId('workspace-row').find((r) => within(r).queryByText(name) !== null);
      return row === undefined ? null : (within(row).queryByTestId('folder-kind-chip')?.textContent ?? null);
    };
    expect(chipOf('repo')).toBe('Git');
    expect(chipOf('tool')).toBe('Code');
    expect(chipOf('papers')).toBe('Documents');
    expect(chipOf('both')).toBeNull();
    expect(chipOf('older-host')).toBeNull();
    expect(screen.getAllByText('Git')).toHaveLength(1);
  });
});
