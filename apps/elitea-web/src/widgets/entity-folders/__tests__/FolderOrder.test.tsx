import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, delay, http } from 'msw';
import { describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient } from '@/shared/api/generated/mutator';
import { EntityCard, type EntityListItem } from '@/shared/ui/EntityCardList';
import { server } from '@/test/setup';

import { useFolderOrder } from '../lib/useFolderOrder';
import { EntityFolderDnd } from '../ui/EntityFolderDnd';
import { EntityFolderScope } from '../ui/EntityFolderScope';

import { BASE, PROJECT, renderInRouter, wireFolder, type FolderFixture } from './harness';

const UPDATE = 'models.applications.skills.update';
const FOLDERS_URL = `${BASE}/elitea_core/entity_folders/prompt_lib/${PROJECT}/skill`;
const ITEMS_URL = `${FOLDERS_URL}/items/:entityId`;

interface Row {
  readonly id: number;
  readonly name: string;
}

// The server's list order (by creation), not the folder's.
const ROWS: readonly Row[] = [
  { id: 13, name: 'Gamma' },
  { id: 12, name: 'Beta' },
  { id: 11, name: 'Alpha' },
];
const SORT_FIELDS = { items: [{ value: 'name', label: 'Name' }], defaultField: 'name' };
const idOf = (row: Row): number => row.id;

/** Folders the server keeps; a PUT with neighbours rearranges them, like the real store does. */
function serveStore(itemIds: readonly number[], refuse = false): { readonly bodies: unknown[] } {
  const folder: { -readonly [K in keyof FolderFixture]: FolderFixture[K] } = { id: '3', name: 'Reviews', position: 1_000_000, item_ids: [...itemIds] };
  const bodies: unknown[] = [];
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(FOLDERS_URL, () => HttpResponse.json({ items: [wireFolder('skill', folder)], pinned_ids: [] })),
    http.put(ITEMS_URL, async ({ params, request }) => {
      const body = (await request.json()) as { neighbor_above_id: number | null; neighbor_below_id: number | null };
      bodies.push(body);
      await delay(30);
      if (refuse) return HttpResponse.json({ error: 'neighbour not in folder' }, { status: 400 });
      const id = Number(params['entityId']);
      const rest = folder.item_ids.filter((item) => item !== id);
      const at = body.neighbor_above_id === null ? 0 : rest.indexOf(body.neighbor_above_id) + 1;
      folder.item_ids = [...rest.slice(0, at), id, ...rest.slice(at)];
      return new HttpResponse(null, { status: 204 });
    }),
  );
  return { bodies };
}

function Page({ folderId = '3' }: { readonly folderId?: string }): React.ReactElement {
  const ordered = useFolderOrder({ projectId: PROJECT, kind: 'skill', folderId, sortFields: SORT_FIELDS, rows: ROWS, idOf });
  const granted = new Set([UPDATE]);
  return (
    <EntityFolderDnd
      projectId={PROJECT}
      kind="skill"
      permissions={granted}
      folderOrder={ordered.folderOrder}
    >
      <output data-testid="sort-fields">{ordered.sortFields.items.map((item) => item.label).join(',')}</output>
      <EntityFolderScope
        projectId={PROJECT}
        kind="skill"
        permissions={granted}
      >
        {ordered.rows.map((row) => {
          const item: EntityListItem = { id: String(row.id), name: row.name, onClick: vi.fn() };
          return (
            <EntityCard
              key={row.id}
              item={item}
            />
          );
        })}
      </EntityFolderScope>
    </EntityFolderDnd>
  );
}

const cardNames = (): string[] =>
  screen.getAllByTestId('entity-card').map((card) => ROWS.find((row) => card.textContent.includes(row.name))?.name ?? '?');

async function openMenu(user: ReturnType<typeof userEvent.setup>, index: number): Promise<void> {
  const card = screen.getAllByTestId('entity-card')[index] as HTMLElement;
  await user.click(within(card).getByTestId('move-to-folder-button'));
}

describe('Folder order', () => {
  it('draws the open folder in its own order and offers "Folder order" first, as the default', async () => {
    serveStore([11, 12, 13]);
    renderInRouter(<Page />);
    await waitFor(() => expect(cardNames()).toEqual(['Alpha', 'Beta', 'Gamma']));
    expect(screen.getByTestId('sort-fields')).toHaveTextContent('Folder order,Name');
    expect(screen.getAllByTestId('entity-drag-handle')[0]).toHaveAttribute('aria-roledescription', 'sortable');
    expect(screen.getAllByTestId('entity-drag-handle')[0]).toHaveAccessibleName('Drag Alpha to a new place or to a folder');
  });

  it('keeps the server order and plain folder handles in the Pinned group', async () => {
    serveStore([11, 12, 13]);
    renderInRouter(<Page folderId="pinned-skill" />);
    await screen.findAllByTestId('entity-drag-handle');
    expect(cardNames()).toEqual(['Gamma', 'Beta', 'Alpha']);
    expect(screen.getByTestId('sort-fields')).toHaveTextContent('Name');
    expect(screen.getAllByTestId('entity-drag-handle')[0]).toHaveAttribute('aria-roledescription', 'draggable');
  });

  it('moves a card down from its menu, sends the displayed neighbours, shows it at once and announces it', async () => {
    const { bodies } = serveStore([11, 12, 13]);
    const user = userEvent.setup();
    renderInRouter(<Page />);
    await waitFor(() => expect(cardNames()).toEqual(['Alpha', 'Beta', 'Gamma']));
    await openMenu(user, 0);
    expect(screen.getByTestId('move-up-option')).toHaveAttribute('aria-disabled', 'true');
    await user.click(screen.getByTestId('move-down-option'));
    // Optimistic: the new order is on screen before the server answers.
    expect(cardNames()).toEqual(['Beta', 'Alpha', 'Gamma']);
    // "Above" is the larger position: Beta, displayed above the drop.
    expect(bodies).toEqual([{ folder_id: '3', neighbor_above_id: 12, neighbor_below_id: 13 }]);
    await waitFor(() => expect(screen.getByTestId('entity-folder-dnd-status')).toHaveTextContent('Moved Alpha to position 2 of 3.'));
    expect(cardNames()).toEqual(['Beta', 'Alpha', 'Gamma']);
  });

  it('moves the last card up to the top with no neighbour above', async () => {
    const { bodies } = serveStore([11, 12, 13]);
    const user = userEvent.setup();
    renderInRouter(<Page />);
    await waitFor(() => expect(cardNames()).toEqual(['Alpha', 'Beta', 'Gamma']));
    await openMenu(user, 2);
    expect(screen.getByTestId('move-down-option')).toHaveAttribute('aria-disabled', 'true');
    await user.click(screen.getByTestId('move-up-option'));
    await openMenu(user, 1);
    await user.click(screen.getByTestId('move-up-option'));
    await waitFor(() => expect(cardNames()).toEqual(['Gamma', 'Alpha', 'Beta']));
    expect(bodies[0]).toEqual({ folder_id: '3', neighbor_above_id: 11, neighbor_below_id: 12 });
    expect(bodies[1]).toEqual({ folder_id: '3', neighbor_above_id: null, neighbor_below_id: 11 });
  });

  it('rolls the order back and says so when the server refuses', async () => {
    serveStore([11, 12, 13], true);
    const user = userEvent.setup();
    renderInRouter(<Page />);
    await waitFor(() => expect(cardNames()).toEqual(['Alpha', 'Beta', 'Gamma']));
    await openMenu(user, 0);
    await user.click(screen.getByTestId('move-down-option'));
    expect(cardNames()).toEqual(['Beta', 'Alpha', 'Gamma']);
    expect(await screen.findByTestId('entity-folder-dnd-error')).toHaveTextContent('neighbour not in folder');
    expect(cardNames()).toEqual(['Alpha', 'Beta', 'Gamma']);
  });
});
