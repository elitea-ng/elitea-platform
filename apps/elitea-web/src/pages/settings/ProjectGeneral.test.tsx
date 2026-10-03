/**
 * Settings › General — who may edit the project identity (#6789).
 *
 * The name and icon writes are the project admin's: the server gates them on
 * `models.project_settings.edit`. An editor of a TEAM project holds only
 * `models.project_context.edit` and must see the header read-only. In the
 * caller's PERSONAL project the server accepts `models.project_context.edit`,
 * so the owner (often an `editor` there) keeps the edit control.
 *
 * The page reads the permission set from the network, so the only double is
 * MSW at that boundary.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '@/test/setup';

import { ProjectGeneral } from './ProjectGeneral';

const BASE = '/api/v2';
const EDIT_LABEL = 'Edit the project icon';

/** Serves the permission set and reports when it has been served. */
function grant(permissions: readonly string[]): () => boolean {
  let served = false;
  server.use(
    http.get(`${BASE}/auth/permissions/prompt_lib/:projectId`, () => {
      served = true;
      return HttpResponse.json(permissions.map((name) => ({ name, enabled: true })));
    }),
  );
  return () => served;
}

function mount(projectName: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <ProjectGeneral projectId="7" projectName={projectName} />
    </QueryClientProvider>,
  );
}

/**
 * The read-only answer is an ABSENT control, which is also what the page shows
 * before the permission set arrives. Wait for the set, let React commit, then
 * read, so the absence is the decision and not the loading state.
 */
async function editControlAfterLoad(served: () => boolean): Promise<HTMLElement | null> {
  await waitFor(() => expect(served()).toBe(true));
  await new Promise((resolve) => setTimeout(resolve, 100));
  return screen.queryByRole('button', { name: EDIT_LABEL });
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/project_info/prompt_lib/:projectId/project-info`, () =>
      HttpResponse.json({ name: 'Demo', icon_meta: null, teammates_count: 3 }),
    ),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('Settings › General — project identity edit gate', () => {
  it('lets the team project admin edit the identity', async () => {
    grant(['models.project_settings.edit', 'models.project_context.edit']);
    mount('Team Alpha');
    expect(await screen.findByRole('button', { name: EDIT_LABEL })).toBeInTheDocument();
  });

  it('shows a team project editor the identity read-only', async () => {
    const served = grant(['models.project_context.edit']);
    mount('Team Alpha');
    expect(await editControlAfterLoad(served)).toBeNull();
  });

  it('lets the owner of a personal project edit its identity as an editor', async () => {
    grant(['models.project_context.edit']);
    mount('project_user_42');
    expect(await screen.findByRole('button', { name: EDIT_LABEL })).toBeInTheDocument();
  });

  it('shows a viewer the identity read-only, even in a personal project', async () => {
    const served = grant(['models.project_context.view']);
    mount('project_user_42');
    expect(await editControlAfterLoad(served)).toBeNull();
  });
});
