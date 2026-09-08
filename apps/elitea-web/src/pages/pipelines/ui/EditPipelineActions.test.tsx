/**
 * The pipeline editor's `⋮` menu: Export and Delete.
 *
 * Both used to be absent — this file's subject (`EditPipelineActions.tsx`)
 * stated that "export and delete have no pipeline-side mount point yet", and
 * the legacy suite's `test_delete_pipeline_via_ui_menu` and
 * `test_export_pipeline_if_available` had no control to drive.
 *
 * Each test asserts the REQUEST that reached the server, or the download the
 * browser was asked to make, never that a menu item exists: a menu item wired
 * to nothing looks exactly like one that works, which is the failure class
 * this app has produced before.
 */
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { ApplicationDetail, ApplicationVersionDetail } from '@/shared/api/generated/model';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';
import { exportNotFound, exportOk } from '@/test/msw/handlers/download';

import { renderPipelinesRoute } from '../__tests__/testRouter';
import { EditPipelineActions } from './EditPipelineActions';

const globals = globalThis as unknown as Record<string, unknown>;
const BASE = '/api/v2';
const PROJECT = 'proj-1';

function setConfig(): void {
  globals['elitea_ui_config'] = {
    vite_server_url: 'https://elitea.example',
    vite_base_uri: '/',
    vite_public_project_id: '1',
  };
  resetConfigForTests();
}

const detail = { id: '7', name: 'Audit Pipeline' } as unknown as ApplicationDetail;
const activeVersion = { id: '11', status: 'draft' } as unknown as ApplicationVersionDetail;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  setConfig();
});

afterEach(() => {
  resetGeneratedClient();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  vi.restoreAllMocks();
});

function renderActions() {
  return renderPipelinesRoute(
    <EditPipelineActions
      applicationId="7"
      detail={detail}
      activeVersion={activeVersion}
      projectId={PROJECT}
      tab="all"
    />,
    '/pipelines/all/7',
    { projectId: PROJECT },
  );
}

async function openMenu(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.click(await screen.findByTestId('pipeline-lifecycle-menu-button'));
}

describe('EditPipelineActions — export', () => {
  it('downloads the pipeline as markdown, following the open version', async () => {
    const captured: { url: string; authorization: string | null }[] = [];
    server.use(exportOk('Audit Pipeline.md', captured));
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);

    const user = userEvent.setup();
    renderActions();
    await openMenu(user);
    await user.click(screen.getByTestId('pipeline-export-menuitem'));

    await waitFor(() => expect(clickSpy).toHaveBeenCalledTimes(1));
    // The markdown branch and the open version are both part of the contract:
    // a request without `format=md` would save a JSON body under a `.md` name.
    expect(captured[0]?.url).toContain('format=md');
    expect(captured[0]?.url).toContain('11');
  });

  it('reports a refused export instead of failing silently', async () => {
    server.use(exportNotFound());
    const user = userEvent.setup();
    renderActions();
    await openMenu(user);
    await user.click(screen.getByTestId('pipeline-export-menuitem'));

    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to export this pipeline.');
  });
});

describe('EditPipelineActions — delete', () => {
  it('asks for the name before it deletes anything', async () => {
    let deleted = 0;
    server.use(
      http.delete(`${BASE}/elitea_core/application/prompt_lib/:projectId/:applicationId`, () => {
        deleted += 1;
        return HttpResponse.json({ ok: true });
      }),
    );
    const user = userEvent.setup();
    renderActions();
    await openMenu(user);
    await user.click(screen.getByTestId('pipeline-delete-menuitem'));

    expect(await screen.findByText('Delete pipeline')).toBeInTheDocument();
    const confirm = screen.getByRole('button', { name: /confirm|delete/i });
    expect(confirm).toBeDisabled();
    expect(deleted).toBe(0);
  });

  it('deletes the pipeline once the name is typed, and leaves the editor', async () => {
    const seen: string[] = [];
    server.use(
      http.delete(`${BASE}/elitea_core/application/prompt_lib/:projectId/:applicationId`, ({ request }) => {
        seen.push(request.url);
        return HttpResponse.json({ ok: true });
      }),
    );
    const user = userEvent.setup();
    const { router } = renderActions();
    await openMenu(user);
    await user.click(screen.getByTestId('pipeline-delete-menuitem'));

    await user.type(await screen.findByRole('textbox'), 'Audit Pipeline');
    await user.click(screen.getByRole('button', { name: /confirm|delete/i }));

    await waitFor(() => expect(seen).toHaveLength(1));
    expect(seen[0]).toContain(`/${PROJECT}/7`);
    // The list tab the editor was opened from — a delete that stayed on the
    // editor of a pipeline that no longer exists is the defect this asserts on.
    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/all'));
  });

  it('reports a refused delete and keeps the reader on the page', async () => {
    server.use(
      http.delete(`${BASE}/elitea_core/application/prompt_lib/:projectId/:applicationId`, () =>
        HttpResponse.json({ error: 'nope' }, { status: 400 }),
      ),
    );
    const user = userEvent.setup();
    const { router } = renderActions();
    await openMenu(user);
    await user.click(screen.getByTestId('pipeline-delete-menuitem'));
    await user.type(await screen.findByRole('textbox'), 'Audit Pipeline');
    await user.click(screen.getByRole('button', { name: /confirm|delete/i }));

    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to delete this pipeline.');
    expect(router.state.location.pathname).toBe('/pipelines/all/7');
  });
});
