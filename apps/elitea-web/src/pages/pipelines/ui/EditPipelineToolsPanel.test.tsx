import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  getGetApplicationMockHandler,
  getListApplicationsMockHandler,
  getUpdateApplicationRelationMockHandler,
} from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';
import { server } from '@/test/setup';

import { renderPipelinesRoute } from '../__tests__/testRouter';
import type { EditPipelineVersionFieldsState } from '../lib/useEditPipelineVersionFields';
import { EditPipelineToolsPanel } from './EditPipelineToolsPanel';

const VERSION = {
  id: '1',
  application_id: '42',
  name: 'base',
  status: 'draft',
  agent_type: 'pipeline',
  tools: [],
  meta: { internal_tools: [] },
} as unknown as ApplicationVersionDetail;

function versionFieldsDouble(overrides: Partial<EditPipelineVersionFieldsState> = {}): EditPipelineVersionFieldsState {
  return {
    fields: { welcomeMessage: '', variables: [], stepLimit: undefined, internalTools: [], llmSettings: undefined, tags: [] },
    applyFieldChange: vi.fn(() => true),
    setTags: vi.fn(),
    isDirty: false,
    markSaved: vi.fn(),
    reset: vi.fn(),
    ...overrides,
  };
}

beforeEach(() => {
  // Without this the generated client has no base URL and every request this
  // panel's `ToolMenu` makes resolves somewhere msw does not serve — which
  // shows up as "Save the pipeline first", not as a network error.
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('EditPipelineToolsPanel', () => {
  /**
   * The prop the agents twin does not pass, and the reason this file exists.
   * `ApplicationTools` narrows the MODULES grid to `attachments` alone for a
   * pipeline (baseline `ApplicationTools.jsx:91-94`); the other internal
   * tools are agent-executor features, so offering their switches here would
   * store `meta.internal_tools` entries the pipeline runtime never reads.
   */
  it('shows only the attachments module, never the agent-executor ones', async () => {
    renderPipelinesRoute(
      <EditPipelineToolsPanel
        projectId="9"
        applicationId={42}
        activeVersion={VERSION}
        versionFields={versionFieldsDouble()}
        isDirty={false}
        isReadOnly={false}
      />,
      '/pipelines/all/42',
      { projectId: '9' },
    );

    const modules = within(await screen.findByText('INTERNAL TOOLS').then((node) => node.parentElement as HTMLElement));
    expect(modules.getByText('Attachments')).toBeInTheDocument();
    expect(modules.queryByText('Python sandbox')).not.toBeInTheDocument();
    expect(modules.queryByText('Swarm Mode')).not.toBeInTheDocument();
  }, 20_000);

  it('routes a module switch into the version-level state the save body reads', async () => {
    const applyFieldChange = vi.fn(() => true);
    renderPipelinesRoute(
      <EditPipelineToolsPanel
        projectId="9"
        applicationId={42}
        activeVersion={VERSION}
        versionFields={versionFieldsDouble({ applyFieldChange })}
        isDirty={false}
        isReadOnly={false}
      />,
      '/pipelines/all/42',
      { projectId: '9' },
    );
    const user = userEvent.setup();

    await screen.findByText('INTERNAL TOOLS');
    // `BaseSwitch` renders a native `<input type="checkbox">` inside an empty
    // `FormControlLabel`, so it has no accessible name of its own — the row's
    // own testid is the handle, exactly as `AgentInternalToolSwitch`'s doc
    // comment says it is meant to be used.
    await user.click(within(screen.getByTestId('internal-tool-attachments')).getByRole('switch'));

    await waitFor(() => expect(applyFieldChange).toHaveBeenCalledWith('version_details.meta.internal_tools', ['attachments']));
  }, 20_000);

  /** A public-project viewer keeps the panel and loses the write affordances. */
  it('renders read-only without crashing when the viewer cannot write', async () => {
    renderPipelinesRoute(
      <EditPipelineToolsPanel
        projectId="9"
        applicationId={42}
        activeVersion={VERSION}
        versionFields={versionFieldsDouble()}
        isDirty={false}
        isReadOnly
      />,
      '/pipelines/all/42',
      { projectId: '9' },
    );

    expect(await screen.findByText('INTERNAL TOOLS')).toBeInTheDocument();
  }, 20_000);

  it('renders before a version has loaded, rather than throwing on the missing ids', async () => {
    renderPipelinesRoute(
      <EditPipelineToolsPanel
        projectId={undefined}
        applicationId={undefined}
        activeVersion={undefined}
        versionFields={versionFieldsDouble()}
        isDirty={false}
        isReadOnly={false}
      />,
      '/pipelines/all/42',
      { projectId: '9' },
    );

    expect(await screen.findByText('INTERNAL TOOLS')).toBeInTheDocument();
  }, 20_000);

  /**
   * The attach REFETCH, which is this file's own third job: `ToolMenu`
   * invalidates its own `getApplication` cache entry, but the page reads the
   * SAME query through `useEditPipelineData` — so without this callback a
   * newly attached toolkit does not appear until a reload.
   */
  it('refetches the pipeline detail after a tool is attached', async () => {
    let detailReads = 0;
    server.use(
      getGetApplicationMockHandler(() => {
        detailReads += 1;
        return {
          id: '42',
          name: 'My Pipeline',
          description: 'A helpful pipeline',
          icon: '',
          owner_id: 'user-1',
          created_at: '2026-01-01T00:00:00Z',
          versions: [{ id: '1', name: 'base', status: 'draft', agent_type: 'pipeline', created_at: '2026-01-01T00:00:00Z' }],
          version_details: { id: '1', application_id: '42', name: 'base', status: 'draft', agent_type: 'pipeline', tools: [] },
        };
      }),
      getListApplicationsMockHandler(info => {
        const url = new URL(info.request.url);
        if (url.searchParams.get('agents_type') !== 'pipeline') return { rows: [], total: 0, page: 0, page_size: 20, total_pages: 0 };
        return {
          rows: [
            {
              id: '9',
              name: 'Other Pipeline',
              owner_id: 'user-1',
              created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-01-01T00:00:00Z',
              is_forked: false,
              meta: {},
              has_interrupt: false,
            },
          ],
          total: 1,
          page: 0,
          page_size: 20,
          total_pages: 1,
        };
      }),
      getUpdateApplicationRelationMockHandler({ application_id: '42', version_id: '1', has_relation: true }),
    );

    renderPipelinesRoute(
      <EditPipelineToolsPanel
        projectId="9"
        applicationId={42}
        activeVersion={VERSION}
        versionFields={versionFieldsDouble()}
        isDirty={false}
        isReadOnly={false}
      />,
      '/pipelines/all/42',
      { projectId: '9' },
    );

    await waitFor(() => expect(screen.getByRole('button', { name: 'Pipeline' })).not.toBeDisabled(), { timeout: 15_000 });
    const readsBeforeAttach = detailReads;
    fireEvent.click(screen.getByRole('button', { name: 'Pipeline' }));
    fireEvent.click(await screen.findByText('Other Pipeline'));

    await waitFor(() => expect(detailReads).toBeGreaterThan(readsBeforeAttach), { timeout: 15_000 });
  }, 20_000);

  it('carries the version attachment toolkit id through to the tool rows', async () => {
    const withAttachmentToolkit = {
      ...VERSION,
      meta: { internal_tools: ['attachments'], attachment_toolkit_id: '77' },
    } as unknown as ApplicationVersionDetail;

    renderPipelinesRoute(
      <EditPipelineToolsPanel
        projectId="9"
        applicationId={42}
        activeVersion={withAttachmentToolkit}
        versionFields={versionFieldsDouble()}
        isDirty={false}
        isReadOnly={false}
      />,
      '/pipelines/all/42',
      { projectId: '9' },
    );

    expect(await screen.findByText('INTERNAL TOOLS')).toBeInTheDocument();
  }, 20_000);
});
