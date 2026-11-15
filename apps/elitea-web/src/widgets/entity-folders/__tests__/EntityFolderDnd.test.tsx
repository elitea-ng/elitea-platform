import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { describe, expect, it, vi } from 'vitest';

import { EntityCard, type EntityListItem } from '@/shared/ui/EntityCardList';
import { server } from '@/test/setup';

import { EntityFolderDnd } from '../ui/EntityFolderDnd';
import { EntityFolderPanel } from '../ui/EntityFolderPanel';
import { EntityFolderScope } from '../ui/EntityFolderScope';
import { MoveSelectionControl } from '../ui/MoveSelectionControl';

import { BASE, PROJECT, renderInRouter, serveFolders } from './harness';

const UPDATE = 'models.applications.skills.update';
const CREATE = 'models.applications.skills.create';
const FOLDERS = [
  { id: '3', name: 'Reviews', position: 2_000_000, item_ids: [11] },
  { id: '1', name: 'Archive', position: 1_000_000, item_ids: [] },
];
const ITEM: EntityListItem = { id: '11', name: 'Reviewer', onClick: vi.fn() };
const ITEMS_URL = `${BASE}/elitea_core/entity_folders/prompt_lib/${PROJECT}/skill/items/:entityId`;
const PINS_URL = `${BASE}/elitea_core/entity_pins/prompt_lib/${PROJECT}/skill/:entityId`;

function page(permissions: readonly string[]): React.ReactElement {
  const granted = new Set(permissions);
  return (
    <EntityFolderDnd
      projectId={PROJECT}
      kind="skill"
      permissions={granted}
    >
      <EntityFolderPanel
        projectId={PROJECT}
        kind="skill"
        permissions={granted}
      />
      <EntityFolderScope
        projectId={PROJECT}
        kind="skill"
        permissions={granted}
      >
        <EntityCard item={ITEM} />
      </EntityFolderScope>
    </EntityFolderDnd>
  );
}

describe('EntityFolderDnd', () => {
  it('gives a card a drag handle named after it, and makes every folder row a drop zone', async () => {
    serveFolders('skill', FOLDERS);
    renderInRouter(page([UPDATE, CREATE]));
    expect(await screen.findByRole('button', { name: 'Drag Reviewer to a folder' })).toBeInTheDocument();
    await waitFor(() => expect(screen.getAllByTestId('entity-folder-drop')).toHaveLength(2));
  });

  it('gives the handle the keyboard drag instructions a screen reader reads on focus', async () => {
    serveFolders('skill', FOLDERS);
    renderInRouter(page([UPDATE]));
    const handle = await screen.findByTestId('entity-drag-handle');
    expect(handle).toHaveAttribute('aria-roledescription', 'draggable');
    const instructionsId = handle.getAttribute('aria-describedby') ?? '';
    expect(document.getElementById(instructionsId)).toHaveTextContent('press Space to pick it up');
  });

  it('draws no handle without the update permission', async () => {
    serveFolders('skill', FOLDERS);
    renderInRouter(page([]));
    await screen.findByTestId('entity-card');
    expect(screen.queryByTestId('entity-drag-handle')).not.toBeInTheDocument();
  });

  it('does not open the card when its handle is clicked', async () => {
    serveFolders('skill', FOLDERS);
    const user = userEvent.setup();
    renderInRouter(page([UPDATE]));
    await user.click(await screen.findByTestId('entity-drag-handle'));
    expect(ITEM.onClick).not.toHaveBeenCalled();
  });
});

describe('Pinned', () => {
  it('shows the caller\'s own count and pins from the card, sending only the pin', async () => {
    serveFolders('skill', FOLDERS, [11, 12]);
    let seen: { id: string; body: unknown } | undefined;
    server.use(
      http.put(PINS_URL, async ({ params, request }) => {
        seen = { id: String(params['entityId']), body: await request.json() };
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const user = userEvent.setup();
    renderInRouter(page([]));
    expect(await screen.findByTestId('entity-folder-pinned')).toHaveTextContent('Pinned');
    await waitFor(() => expect(screen.getByTestId('entity-folder-pinned')).toHaveTextContent('2'));
    const toggle = await screen.findByRole('button', { name: 'Unpin Reviewer' });
    expect(toggle).toHaveAttribute('aria-pressed', 'true');
    await user.click(toggle);
    await waitFor(() => expect(seen).toEqual({ id: '11', body: { pinned: false } }));
    expect(ITEM.onClick).not.toHaveBeenCalled();
  });

  it('selects the Pinned group of this list through the same search param a folder uses', async () => {
    serveFolders('skill', FOLDERS, [11]);
    const user = userEvent.setup();
    const view = renderInRouter(page([]));
    await user.click(await screen.findByTestId('entity-folder-pinned'));
    await waitFor(() => expect(view.router.state.location.search).toMatchObject({ entity_folder: 'pinned-skill' }));
    // The reserved value is not a folder id: the panel must not clear it as stale.
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(view.router.state.location.search).toMatchObject({ entity_folder: 'pinned-skill' });
  });
});

describe('MoveSelectionControl', () => {
  function control(permissions: readonly string[], onSettled = vi.fn()): React.ReactElement {
    return (
      <MoveSelectionControl
        projectId={PROJECT}
        kind="skill"
        permissions={new Set(permissions)}
        ids={['11', '12']}
        onSettled={onSettled}
        testIdPrefix="skill"
      />
    );
  }

  it('moves every selected row into the chosen folder and reports none failed', async () => {
    serveFolders('skill', FOLDERS);
    const moved: string[] = [];
    server.use(
      http.put(ITEMS_URL, async ({ params, request }) => {
        moved.push(`${String(params['entityId'])}:${JSON.stringify(await request.json())}`);
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const onSettled = vi.fn();
    const user = userEvent.setup();
    renderInRouter(control([UPDATE], onSettled));
    await user.click(await screen.findByTestId('skill-selection-move'));
    const options = await screen.findAllByTestId('move-selection-option');
    expect(options.map((option) => option.textContent)).toEqual(['Reviews', 'Archive']);
    await user.click(options[1] as HTMLElement);
    await waitFor(() => expect(onSettled).toHaveBeenCalledWith({ failed: [] }));
    // Reverse selection order, so the folder reads in the order the rows were selected.
    expect(moved).toEqual(['12:{"folder_id":"1"}', '11:{"folder_id":"1"}']);
  });

  it('takes the selected rows that sit in a folder out of it with a null folder', async () => {
    serveFolders('skill', FOLDERS);
    const bodies: unknown[] = [];
    server.use(
      http.put(ITEMS_URL, async ({ request }) => {
        bodies.push(await request.json());
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const user = userEvent.setup();
    renderInRouter(control([UPDATE]));
    await user.click(await screen.findByTestId('skill-selection-move'));
    await user.click(await screen.findByTestId('move-selection-root-option'));
    // 12 sits at the root already: only 11 leaves its folder.
    await waitFor(() => expect(bodies).toEqual([{ folder_id: null }]));
  });

  it('keeps only the refused rows selected', async () => {
    serveFolders('skill', FOLDERS);
    server.use(
      http.put(ITEMS_URL, ({ params }) =>
        params['entityId'] === '12' ? HttpResponse.json({ error: 'skill not found' }, { status: 404 }) : new HttpResponse(null, { status: 204 }),
      ),
    );
    const onSettled = vi.fn();
    const user = userEvent.setup();
    renderInRouter(control([UPDATE], onSettled));
    await user.click(await screen.findByTestId('skill-selection-move'));
    await user.click((await screen.findAllByTestId('move-selection-option'))[0] as HTMLElement);
    await waitFor(() => expect(onSettled).toHaveBeenCalledWith({ failed: ['12'] }));
  });

  it('does not re-move a row that is already in the chosen folder', async () => {
    serveFolders('skill', FOLDERS);
    const moved: string[] = [];
    server.use(
      http.put(ITEMS_URL, ({ params }) => {
        moved.push(String(params['entityId']));
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const onSettled = vi.fn();
    const user = userEvent.setup();
    renderInRouter(control([UPDATE], onSettled));
    await user.click(await screen.findByTestId('skill-selection-move'));
    // Reviews already holds 11.
    await user.click((await screen.findAllByTestId('move-selection-option'))[0] as HTMLElement);
    await waitFor(() => expect(onSettled).toHaveBeenCalledWith({ failed: [] }));
    expect(moved).toEqual(['12']);
  });

  it('draws nothing without the update permission', async () => {
    serveFolders('skill', FOLDERS);
    renderInRouter(control([]));
    await waitFor(() => expect(screen.queryByTestId('skill-selection-move')).not.toBeInTheDocument());
  });
});
