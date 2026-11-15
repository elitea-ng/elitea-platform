import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { SidePanelHarness } from '@/test/sidePanelHarness';

import { Skills } from './Skills';
import { renderSkillsRoute } from './__tests__/testRouter';

const BASE = '/api/v2';
const CREATE = 'models.applications.applications.create';
const skill = {
  id: 'skill-1',
  project_id: 'project-1',
  name: 'Reviewer',
  description: 'Reviews code',
  type: 'skill',
  is_default: false,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/skills/prompt_lib/:projectId`, () =>
      HttpResponse.json({ items: [skill], total: 1, page: 1, page_size: 20, total_pages: 1 }),
    ),
  );
});
afterEach(() => {
  resetGeneratedClient();
  vi.restoreAllMocks();
});

describe('Skills page', () => {
  it('loads skills and navigates to create or detail routes', async () => {
    const user = userEvent.setup();
    const { router } = renderSkillsRoute(<Skills />);
    expect(await screen.findByText('Reviewer')).toBeInTheDocument();
    await user.click(screen.getByTestId('entity-card'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/skills/all/skill-1'));
  });

  it('navigates to the create route from the primary action', async () => {
    server.use(getPermissionListMockHandler([{ name: CREATE, enabled: true }]));
    const user = userEvent.setup();
    const { router } = renderSkillsRoute(<Skills />);
    await user.click(await screen.findByRole('button', { name: 'New skill' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/skills/create'));
  });

  it('deletes after confirmation and refreshes the list', async () => {
    const user = userEvent.setup();
    let deleted = false;
    server.use(
      http.delete(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId`, () => {
        deleted = true;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    renderSkillsRoute(<Skills />);
    await user.click(await screen.findByRole('button', { name: 'Delete' }));
    await user.click(screen.getByRole('button', { name: 'Delete', hidden: false }));
    await waitFor(() => expect(deleted).toBe(true));
  });

  it('exports Markdown through a browser download', async () => {
    const user = userEvent.setup();
    server.use(
      http.get(`${BASE}/elitea_core/skill_export/prompt_lib/:projectId/:skillId`, () =>
        HttpResponse.text('# Reviewer'),
      ),
    );
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:skill');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    renderSkillsRoute(<Skills />);
    await user.click(await screen.findByRole('button', { name: 'Export' }));
    await waitFor(() => expect(click).toHaveBeenCalled());
  });

  /**
   * COMPOSITION ROOT — the shell's breadcrumb names the page; the page draws no
   * toolbar row (ADR-0051): the Skills | Public switch, import and
   * "+ New skill" for a caller who may create are in the right sidebar.
   */
  it('renders its mode switch, import and create controls in the List tab, and no heading of its own', async () => {
    server.use(getPermissionListMockHandler([{ name: CREATE, enabled: true }]));
    renderSkillsRoute(<SidePanelHarness><Skills /></SidePanelHarness>);

    const tab = await screen.findByTestId('side-panel-tab-list');
    expect(await within(tab).findByRole('button', { name: 'New skill' })).toBeInTheDocument();
    expect(within(tab).getByTestId('skills-mode-public')).toBeInTheDocument();
    expect(screen.queryByTestId('page-toolbar')).toBeNull();
    expect(screen.queryByRole('heading', { level: 1 })).toBeNull();
  });

  it('offers no "+ New skill" to a caller who may not create', async () => {
    server.use(getPermissionListMockHandler([]));
    renderSkillsRoute(<Skills />);

    expect(await screen.findByText('Reviewer')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'New skill' })).toBeNull();
  });
});
