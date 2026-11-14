import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vitest';

import { server } from '@/test/setup';

import { EntityFolderPanel } from '../ui/EntityFolderPanel';

import { BASE, PROJECT, renderInRouter, serveFolders, wireFolder } from './harness';

const CREATE = 'models.applications.skills.create';
const UPDATE = 'models.applications.skills.update';
const FOLDERS = [
  { id: '3', name: 'Reviews', position: 3_000_000, item_ids: [11, 12] },
  { id: '2', name: 'Drafts', position: 2_000_000, item_ids: [] },
  { id: '1', name: 'Archive', position: 1_000_000, item_ids: [13] },
];
const URL = `${BASE}/elitea_core/entity_folders/prompt_lib/${PROJECT}/skill`;

function panel(permissions: readonly string[]): React.ReactElement {
  return (
    <EntityFolderPanel
      projectId={PROJECT}
      kind="skill"
      permissions={new Set(permissions)}
    />
  );
}

describe('EntityFolderPanel', () => {
  it('lists the project folders in the server order with their entity counts', async () => {
    serveFolders('skill', FOLDERS);
    renderInRouter(panel([]));
    const rows = await screen.findAllByTestId('entity-folder-row');
    expect(rows.map((row) => row.textContent)).toEqual(['Reviews2', 'Drafts0', 'Archive1']);
  });

  it('opens a folder with one click, writing it to the entity_folder param, and All clears it', async () => {
    serveFolders('skill', FOLDERS);
    const user = userEvent.setup();
    const { router } = renderInRouter(panel([]));
    await user.click((await screen.findAllByTestId('entity-folder-row'))[1] as HTMLElement);
    await waitFor(() => expect(router.state.location.search).toMatchObject({ entity_folder: '2' }));
    expect(screen.getAllByTestId('entity-folder-row')[1]).toHaveClass('Mui-selected');
    await user.click(screen.getByTestId('entity-folder-all'));
    await waitFor(() => expect(router.state.location.search).not.toHaveProperty('entity_folder', '2'));
  });

  it('drops an open folder that is not in this list, so a folder id of another list cannot empty it', async () => {
    serveFolders('skill', FOLDERS);
    const { router } = renderInRouter(panel([]), '/skills/all?entity_folder=99');
    await screen.findAllByTestId('entity-folder-row');
    await waitFor(() => expect(router.state.location.search).not.toHaveProperty('entity_folder', '99'));
  });

  it('shows no create control and no row menu without the list permissions', async () => {
    serveFolders('skill', FOLDERS);
    renderInRouter(panel([]));
    await screen.findAllByTestId('entity-folder-row');
    expect(screen.queryByTestId('entity-folder-create')).not.toBeInTheDocument();
    expect(screen.queryByTestId('entity-folder-more')).not.toBeInTheDocument();
  });

  it('creates a folder with the create permission and refetches the list', async () => {
    serveFolders('skill', []);
    let body: unknown;
    server.use(
      http.post(URL, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(wireFolder('skill', { id: '4', name: 'Fresh', position: 1, item_ids: [] }), { status: 201 });
      }),
    );
    const user = userEvent.setup();
    renderInRouter(panel([CREATE]));
    expect(await screen.findByTestId('entity-folder-empty')).toBeInTheDocument();
    await user.click(screen.getByTestId('entity-folder-create'));
    await user.type(await screen.findByTestId('entity-folder-name-input'), '  Fresh ');
    await user.click(within(screen.getByTestId('entity-folder-name-dialog')).getByRole('button', { name: 'Create' }));
    await waitFor(() => expect(body).toEqual({ name: 'Fresh' }));
  });

  it('shows the server refusal of a duplicate name inside the dialog and keeps it open', async () => {
    serveFolders('skill', FOLDERS);
    server.use(http.post(URL, () => HttpResponse.json({ error: 'a folder with this name already exists in this list' }, { status: 409 })));
    const user = userEvent.setup();
    renderInRouter(panel([CREATE]));
    await user.click(await screen.findByTestId('entity-folder-create'));
    await user.type(await screen.findByTestId('entity-folder-name-input'), 'drafts');
    await user.click(within(screen.getByTestId('entity-folder-name-dialog')).getByRole('button', { name: 'Create' }));
    expect(await screen.findByText('a folder with this name already exists in this list')).toBeInTheDocument();
    expect(screen.getByTestId('entity-folder-name-dialog')).toBeInTheDocument();
  });

  it('renames through PUT with the name alone, so the order is untouched', async () => {
    serveFolders('skill', FOLDERS);
    let seen: { url: string; body: unknown } | undefined;
    server.use(
      http.put(`${URL}/:folderId`, async ({ request }) => {
        seen = { url: request.url, body: await request.json() };
        return HttpResponse.json(wireFolder('skill', { id: '2', name: 'Wip', position: 2_000_000, item_ids: [] }));
      }),
    );
    const user = userEvent.setup();
    renderInRouter(panel([UPDATE]));
    await screen.findAllByTestId('entity-folder-row');
    await user.click(screen.getAllByTestId('entity-folder-more')[1] as HTMLElement);
    await user.click(await screen.findByRole('menuitem', { name: 'Rename' }));
    const input = await screen.findByTestId('entity-folder-name-input');
    expect(input).toHaveValue('Drafts');
    await user.clear(input);
    await user.type(input, 'Wip');
    await user.click(within(screen.getByTestId('entity-folder-name-dialog')).getByRole('button', { name: 'Rename' }));
    await waitFor(() => expect(seen?.body).toEqual({ name: 'Wip' }));
    expect(seen?.url).toContain('/skill/2');
  });

  it('reorders with the neighbour ids, not a position number', async () => {
    serveFolders('skill', FOLDERS);
    let body: unknown;
    server.use(
      http.put(`${URL}/:folderId`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(wireFolder('skill', FOLDERS[0] as never));
      }),
    );
    const user = userEvent.setup();
    renderInRouter(panel([UPDATE]));
    await screen.findAllByTestId('entity-folder-row');
    // Reviews is on top: it cannot go up, and going down drops it between Drafts and Archive.
    await user.click(screen.getAllByTestId('entity-folder-more')[0] as HTMLElement);
    expect(await screen.findByRole('menuitem', { name: 'Move up' })).toHaveAttribute('aria-disabled', 'true');
    await user.click(screen.getByRole('menuitem', { name: 'Move down' }));
    await waitFor(() => expect(body).toEqual({ neighbor_above_id: '2', neighbor_below_id: '1' }));
  });

  it('deletes after confirmation, says nothing but the folder goes, and leaves the open folder when it was the one deleted', async () => {
    serveFolders('skill', FOLDERS);
    let deleted: string | undefined;
    server.use(
      http.delete(`${URL}/:folderId`, ({ params }) => {
        deleted = String(params['folderId']);
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const user = userEvent.setup();
    const { router } = renderInRouter(panel([UPDATE]), '/skills/all?entity_folder=2');
    await screen.findAllByTestId('entity-folder-row');
    await user.click(screen.getAllByTestId('entity-folder-more')[1] as HTMLElement);
    await user.click(await screen.findByRole('menuitem', { name: 'Delete…' }));
    const dialog = await screen.findByTestId('entity-folder-delete-dialog');
    expect(dialog).toHaveTextContent('nothing else is deleted');
    await user.click(within(dialog).getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(deleted).toBe('2'));
    await waitFor(() => expect(router.state.location.search).not.toHaveProperty('entity_folder', '2'));
  });

  it('draws nothing when disabled, and asks the server for nothing', async () => {
    let asked = false;
    server.use(
      http.get(URL, () => {
        asked = true;
        return HttpResponse.json({ items: [] });
      }),
    );
    renderInRouter(
      <EntityFolderPanel
        enabled={false}
        projectId={PROJECT}
        kind="skill"
        permissions={new Set()}
      />,
    );
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(screen.queryByTestId('entity-folder-panel')).not.toBeInTheDocument();
    expect(asked).toBe(false);
  });
});
