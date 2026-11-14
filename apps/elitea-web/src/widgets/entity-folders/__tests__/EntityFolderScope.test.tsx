import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { describe, expect, it, vi } from 'vitest';

import { EntityCard, EntityListTable, type EntityListItem } from '@/shared/ui/EntityCardList';
import { server } from '@/test/setup';

import { EntityFolderScope } from '../ui/EntityFolderScope';

import { BASE, PROJECT, renderInRouter, serveFolders } from './harness';

const UPDATE = 'models.applications.skills.update';
const FOLDERS = [
  { id: '3', name: 'Reviews', position: 2_000_000, item_ids: [11] },
  { id: '1', name: 'Archive', position: 1_000_000, item_ids: [] },
];
const ITEM: EntityListItem = { id: '11', name: 'Reviewer', onClick: vi.fn() };
const OTHER: EntityListItem = { id: '12', name: 'Summariser' };
const MOVE_URL = `${BASE}/elitea_core/entity_folders/prompt_lib/${PROJECT}/skill/items/:entityId`;

function scoped(permissions: readonly string[], child: React.ReactElement): React.ReactElement {
  return (
    <EntityFolderScope
      projectId={PROJECT}
      kind="skill"
      permissions={new Set(permissions)}
    >
      {child}
    </EntityFolderScope>
  );
}

describe('EntityFolderScope on cards', () => {
  it('puts "Move to folder" on a card, ticks the current folder and moves the entity on a choice', async () => {
    serveFolders('skill', FOLDERS);
    let seen: { id: string; body: unknown } | undefined;
    server.use(
      http.put(MOVE_URL, async ({ params, request }) => {
        seen = { id: String(params['entityId']), body: await request.json() };
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const user = userEvent.setup();
    renderInRouter(scoped([UPDATE], <EntityCard item={ITEM} />));
    await user.click(await screen.findByRole('button', { name: 'Move Reviewer to a folder' }));
    const options = await screen.findAllByTestId('move-to-folder-option');
    expect(options.map((option) => option.textContent)).toEqual(['Reviews', 'Archive']);
    expect(options[0]).toHaveClass('Mui-selected');
    await user.click(options[1] as HTMLElement);
    await waitFor(() => expect(seen).toEqual({ id: '11', body: { folder_id: '1' } }));
    // The menu click must not open the card behind it.
    expect(ITEM.onClick).not.toHaveBeenCalled();
  });

  it('offers a way back to the root only for an entity that sits in a folder, and sends null', async () => {
    serveFolders('skill', FOLDERS);
    const user = userEvent.setup();
    renderInRouter(scoped([UPDATE], <EntityCard item={OTHER} />));
    await user.click(await screen.findByRole('button', { name: 'Move Summariser to a folder' }));
    await screen.findAllByTestId('move-to-folder-option');
    expect(screen.queryByTestId('move-to-root-option')).not.toBeInTheDocument();
  });

  it('sends folder_id null when an entity leaves its folder', async () => {
    serveFolders('skill', FOLDERS);
    let body: unknown;
    server.use(
      http.put(MOVE_URL, async ({ request }) => {
        body = await request.json();
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const user = userEvent.setup();
    renderInRouter(scoped([UPDATE], <EntityCard item={ITEM} />));
    await user.click(await screen.findByRole('button', { name: 'Move Reviewer to a folder' }));
    await user.click(await screen.findByTestId('move-to-root-option'));
    await waitFor(() => expect(body).toEqual({ folder_id: null }));
  });

  it('draws no control without the update permission', async () => {
    serveFolders('skill', FOLDERS);
    renderInRouter(scoped([], <EntityCard item={ITEM} />));
    await screen.findByTestId('entity-card');
    expect(screen.queryByTestId('move-to-folder-button')).not.toBeInTheDocument();
  });

  it('draws nothing extra on a card outside any scope', async () => {
    renderInRouter(<EntityCard item={ITEM} />);
    await screen.findByTestId('entity-card');
    expect(screen.queryByTestId('move-to-folder-button')).not.toBeInTheDocument();
  });
});

describe('EntityFolderScope on the table', () => {
  it('adds a Folder column that names the folder and carries the same control', async () => {
    serveFolders('skill', FOLDERS);
    const user = userEvent.setup();
    renderInRouter(scoped([UPDATE], <EntityListTable items={[ITEM, OTHER]} />));
    expect(await screen.findByRole('columnheader', { name: 'Folder' })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByTestId('entity-folder-cell-name')).toHaveTextContent('Reviews'));
    const rows = screen.getAllByTestId('entity-list-row');
    expect(within(rows[1] as HTMLElement).queryByTestId('entity-folder-cell-name')).not.toBeInTheDocument();
    await user.click(within(rows[1] as HTMLElement).getByTestId('move-to-folder-button'));
    expect(await screen.findAllByTestId('move-to-folder-option')).toHaveLength(2);
    // The cell sits inside a clickable row: using it never opens the entity.
    expect(ITEM.onClick).not.toHaveBeenCalled();
  });

  it('shows the folder read-only without the update permission', async () => {
    serveFolders('skill', FOLDERS);
    renderInRouter(scoped([], <EntityListTable items={[ITEM]} />));
    await waitFor(() => expect(screen.getByTestId('entity-folder-cell-name')).toHaveTextContent('Reviews'));
    expect(screen.queryByTestId('move-to-folder-button')).not.toBeInTheDocument();
  });

  it('has no Folder column outside a scope', async () => {
    renderInRouter(<EntityListTable items={[ITEM]} />);
    await screen.findByTestId('entity-list-row');
    expect(screen.queryByRole('columnheader', { name: 'Folder' })).not.toBeInTheDocument();
  });
});
