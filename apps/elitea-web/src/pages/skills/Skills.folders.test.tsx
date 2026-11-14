import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { SidePanelHarness } from '@/test/sidePanelHarness';

import { Skills } from './Skills';
import { renderSkillsRoute } from './__tests__/testRouter';

const BASE = '/api/v2';
const FOLDER_URL = `${BASE}/elitea_core/entity_folders/prompt_lib/project-1/skill`;

function makeSkill(id: string, name: string) {
  return {
    id,
    project_id: 'project-1',
    name,
    description: 'A skill',
    type: 'skill',
    is_default: false,
    tags: [],
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/skills/prompt_lib/:projectId`, () =>
      HttpResponse.json({ items: [makeSkill('11', 'Reviewer'), makeSkill('12', 'Summariser')], total: 2, page: 1, page_size: 20, total_pages: 1 }),
    ),
    http.get(FOLDER_URL, () =>
      HttpResponse.json({
        items: [{ id: '5', kind: 'skill', name: 'Work', position: 1_000_000, item_ids: [11], created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' }],
      }),
    ),
    getPermissionListMockHandler([
      { name: 'models.applications.skills.update', enabled: true },
      { name: 'models.applications.skills.create', enabled: true },
    ]),
  );
});
afterEach(() => {
  resetGeneratedClient();
});

describe('Skills folders', () => {
  it('opens a folder from the Filters panel and narrows the list to what is in it', async () => {
    const user = userEvent.setup();
    const { router } = renderSkillsRoute(
      <SidePanelHarness>
        <Skills />
      </SidePanelHarness>,
    );
    expect(await screen.findAllByTestId('entity-card')).toHaveLength(2);
    await user.click(await screen.findByTestId('entity-folder-row'));
    await waitFor(() => expect(screen.getAllByTestId('entity-card')).toHaveLength(1));
    expect(within(screen.getByTestId('entity-card')).getByText('Reviewer')).toBeInTheDocument();
    expect(router.state.location.search).toMatchObject({ entity_folder: '5' });
    // Back to every skill.
    await user.click(screen.getByTestId('entity-folder-all'));
    await waitFor(() => expect(screen.getAllByTestId('entity-card')).toHaveLength(2));
  });

  it('moves a skill into a folder from its card', async () => {
    let body: unknown;
    let url = '';
    server.use(
      http.put(`${FOLDER_URL}/items/:entityId`, async ({ request }) => {
        url = request.url;
        body = await request.json();
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const user = userEvent.setup();
    renderSkillsRoute(<Skills />);
    await user.click(await screen.findByRole('button', { name: 'Move Summariser to a folder' }));
    await user.click(await screen.findByTestId('move-to-folder-option'));
    await waitFor(() => expect(body).toEqual({ folder_id: '5' }));
    expect(url).toContain('/skill/items/12');
  });

  it('shows no folder controls to a person without the list permissions', async () => {
    server.use(getPermissionListMockHandler([]));
    renderSkillsRoute(
      <SidePanelHarness>
        <Skills />
      </SidePanelHarness>,
    );
    await screen.findAllByTestId('entity-card');
    expect(screen.queryByTestId('move-to-folder-button')).not.toBeInTheDocument();
    expect(screen.queryByTestId('entity-folder-create')).not.toBeInTheDocument();
  });
});
