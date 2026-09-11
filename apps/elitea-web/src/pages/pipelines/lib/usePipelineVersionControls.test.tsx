import { http, HttpResponse } from 'msw';
import { QueryClient } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { useForm } from 'react-hook-form';
import { beforeEach, describe, expect, it } from 'vitest';

import type { ApplicationCreationInput } from '@/entities/application-form';
import type { PipelineGraphDraft } from '@/features/pipelines';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getGetApplicationQueryKey } from '@/shared/api/generated/applications/applications';
import type { ApplicationVersionDetail, ApplicationVersionSummary } from '@/shared/api/generated/model';
import { server } from '@/test/setup';
import { useNavBlockerStore } from '@/widgets/app-shell';

import { renderPipelinesRoute } from '../__tests__/testRouter';

import { usePipelineVersionControls } from './usePipelineVersionControls';

/**
 * Driven through this unit's REAL router fixture rather than a mocked
 * `useNavigate` — R-M1 (`elitea/no-vi-mock`) allows only the MSW network
 * boundary to be substituted, and a mocked navigate would in any case prove
 * only that the hook called a function, not that the route it names resolves.
 * Asserting `router.state.location.pathname` proves the latter, which matters
 * here: `/pipelines/:tab/:agentId/:version` is the whole mechanism by which
 * choosing a version LOADS that version's graph.
 */
const versions: readonly ApplicationVersionSummary[] = [
  { id: '1', name: 'base', status: 'draft', agent_type: 'pipeline', created_at: '2026-01-01T00:00:00Z' },
  { id: '2', name: 'v1', status: 'draft', agent_type: 'pipeline', created_at: '2026-01-02T00:00:00Z' },
];

const activeVersion = {
  id: '1',
  application_id: '42',
  name: 'base',
  status: 'draft',
  agent_type: 'pipeline',
  instructions: 'entry_point: LLM_1\nnodes: []\n',
  meta: { step_limit: 40, internal_tools: [] },
} as unknown as ApplicationVersionDetail;

const ADMITTED = { document: {}, parseFailed: false, issues: [], hasGraph: true, isAdmissible: true } as const;

const liveGraph: PipelineGraphDraft = {
  instructions: 'entry_point: LLM_9\nnodes:\n  - id: LLM_9\n    type: llm\n',
  admission: ADMITTED,
  pipelineSettings: {
    nodes: [{ id: 'LLM_9', position: { x: 120, y: 240 } }],
    edges: [],
    orientation: 'vertical',
    layout_version: '1.0',
  },
};

/** The same graph, but one the native runtime would refuse — `readGraphDraft` reports that verdict alongside the document it is about to store. */
const inadmissibleGraph: PipelineGraphDraft = {
  ...liveGraph,
  admission: { ...ADMITTED, isAdmissible: false, issues: [] },
};

interface ProbeProps {
  readonly readGraphDraft: () => PipelineGraphDraft | undefined;
  readonly version?: ApplicationVersionDetail | undefined;
  readonly isGraphAdmissible?: boolean;
}

function Probe({ readGraphDraft, version = activeVersion, isGraphAdmissible = true }: ProbeProps) {
  // The active version is LOCAL state, not a prop, because the fixture bakes
  // this element into the router's route component — a `rerender` of the tree
  // above would not reach it.
  const [current, setCurrent] = useState(version);
  const form = useForm<ApplicationCreationInput>({
    values: { name: 'p', description: 'd', version_details: { conversation_starters: ['live starter'] } },
  });
  const state = usePipelineVersionControls({
    projectId: '9',
    applicationId: 42,
    tab: 'my',
    versions,
    activeVersion: current,
    control: form.control,
    versionFields: { welcomeMessage: '', variables: [], stepLimit: undefined, internalTools: [], llmSettings: undefined, tags: [] },
    readGraphDraft,
    isReadOnly: false,
    isFetching: false,
    isGraphAdmissible,
  });
  return (
    <div>
      <span data-testid="body">{JSON.stringify(state.versionBody)}</span>
      <span data-testid="error">{String(state.versionError)}</span>
      <span data-testid="can-save-version">{String(state.canSaveNewVersion)}</span>
      <span data-testid="save-version-blocked">{String(state.isSaveNewVersionBlocked)}</span>
      <button
        data-testid="switch-active-version"
        onClick={() => setCurrent({ ...activeVersion, id: '2' })}
      >
        switch active version
      </button>
      <button
        data-testid="delete-failed"
        onClick={() => state.versionDelete?.onVersionDeleteError('Published version can not be updated/deleted.')}
      >
        delete failed
      </button>
      <button
        data-testid="select"
        onClick={() => state.handleSelectVersion({ id: 2, name: 'v1' })}
      >
        select
      </button>
      <button
        data-testid="saved"
        onClick={() => state.handleNewVersionSaved({ id: '7' } as ApplicationVersionDetail)}
      >
        saved
      </button>
      <button
        data-testid="deleted"
        onClick={() => state.versionDelete?.onVersionDeleted()}
      >
        deleted
      </button>
    </div>
  );
}

/** Every PUT this hook's graph-carry issues, in order. */
function captureVersionPuts(): { url: string; body: Record<string, unknown> }[] {
  const puts: { url: string; body: Record<string, unknown> }[] = [];
  server.use(
    http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
      puts.push({ url: request.url, body: (await request.json()) as Record<string, unknown> });
      return HttpResponse.json({ id: '7', application_id: '42', name: 'v2', status: 'draft' }, { status: 200 });
    }),
  );
  return puts;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  useNavBlockerStore.getState().setBlockNav(false);
  return () => {
    resetGeneratedClient();
    useNavBlockerStore.getState().setBlockNav(false);
  };
});

describe('usePipelineVersionControls', () => {
  it('switching versions navigates onto the version route, which is what re-seeds the graph', async () => {
    const { router } = renderPipelinesRoute(<Probe readGraphDraft={() => undefined} />, '/pipelines/my/42');
    await userEvent.setup().click(await screen.findByTestId('select'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/my/42/2'));
  });

  it('deleting the open version leaves for the pipeline default-version route', async () => {
    const { router } = renderPipelinesRoute(<Probe readGraphDraft={() => undefined} />, '/pipelines/my/42/1');
    await userEvent.setup().click(await screen.findByTestId('deleted'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/my/42'));
  });

  /**
   * This caller's POST uses stored instructions. Its follow-up PUT must
   * preserve the live canvas, even though Main now accepts geometry on POST.
   */
  it('carries the LIVE graph onto the created version with a PUT aimed at its id', async () => {
    const puts = captureVersionPuts();
    const { router } = renderPipelinesRoute(<Probe readGraphDraft={() => liveGraph} />, '/pipelines/my/42/1');

    await userEvent.setup().click(await screen.findByTestId('saved'));

    await waitFor(() => expect(puts).toHaveLength(1));
    // Aimed at the id the POST just minted (7), not at the version that was open (1).
    expect(puts[0]?.url).toContain('/version/prompt_lib/9/42/7');
    expect(puts[0]?.body['instructions']).toBe(liveGraph.instructions);
    const settings = puts[0]?.body['pipeline_settings'] as Record<string, unknown>;
    expect(settings['nodes']).toEqual([{ id: 'LLM_9', position: { x: 120, y: 240 } }]);
    expect(settings['layout_version']).toBe('1.0');

    // …and the navigation happens AFTER the carry, so the editor re-seeds
    // from a version that already holds the graph.
    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/my/42/7'));
  });

  /**
   * `usePipelineGraphDraft` returns `undefined` when the editor holds nothing
   * to save (not mounted, or not seeded yet). Writing then would blank a real
   * stored pipeline — the louder version of the data loss #135 removed.
   */
  it('sends no PUT at all when the flow editor holds nothing to carry', async () => {
    const puts = captureVersionPuts();
    const { router } = renderPipelinesRoute(<Probe readGraphDraft={() => undefined} />, '/pipelines/my/42/1');

    await userEvent.setup().click(await screen.findByTestId('saved'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/my/42/7'));
    expect(puts).toHaveLength(0);
  });

  /**
   * A failed carry must not read as a clean save. The version really was
   * created, so the navigation still happens; the refusal is reported instead
   * of swallowed (this app has no toast infrastructure).
   */
  it('reports a failed graph carry and still lands on the created version', async () => {
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', () =>
        HttpResponse.json({ error: 'nope' }, { status: 500 }),
      ),
    );
    const { router } = renderPipelinesRoute(<Probe readGraphDraft={() => liveGraph} />, '/pipelines/my/42/1');

    await userEvent.setup().click(await screen.findByTestId('saved'));

    await waitFor(() =>
      expect(screen.getByTestId('error')).toHaveTextContent(
        'The new version was created, but its flow graph could not be copied onto it.',
      ),
    );
    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/my/42/7'));
  });

  /**
   * The dropdown's source is the application-detail response, which is stale
   * by exactly the new entry the moment the POST resolves — `useSaveNewVersion`
   * deliberately invalidates nothing.
   */
  it('invalidates the application detail so the new version appears in the dropdown', async () => {
    captureVersionPuts();
    // `gcTime: Infinity`, not the fixture's default 0: an unobserved entry is
    // collected the moment it is written, and `getQueryState` would then
    // answer `undefined` whether the invalidation happened or not — a test
    // that cannot fail for the right reason.
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
    const key = getGetApplicationQueryKey('9', 42);
    queryClient.setQueryData(key, { data: { id: '42', versions: [] } });

    renderPipelinesRoute(<Probe readGraphDraft={() => liveGraph} />, '/pipelines/my/42/1', { queryClient });
    await userEvent.setup().click(await screen.findByTestId('saved'));

    await waitFor(() => expect(queryClient.getQueryState(key)?.isInvalidated).toBe(true));
  });

  it('clones the live conversation starters and the pipeline agent_type into the create body', async () => {
    renderPipelinesRoute(<Probe readGraphDraft={() => undefined} />, '/pipelines/my/42/1');
    const body = JSON.parse((await screen.findByTestId('body')).textContent ?? '{}') as Record<string, unknown>;

    expect(body['agent_type']).toBe('pipeline');
    expect(body['conversation_starters']).toEqual(['live starter']);
    expect(body['meta']).toEqual({ step_limit: 40, internal_tools: [] });
  });
  /**
   * #133 + the version bar. `EditPipeline` arms the app-wide guard off its own
   * dirty state, and `NavBlockerDialog`'s `shouldBlockFn` blocks ANY pathname
   * change while it is raised — including the two this hook owns. The fixture
   * mounts the real dialog (`withNavBlocker`) because a router without it
   * cannot fail: that absence is exactly why the unit suite never saw this.
   *
   * Red without `disarmUnsavedChangesNavBlocker()` in `finishNewVersion`: the
   * pathname stays on `/pipelines/my/42/1` with the guard's dialog rendered,
   * asking the user whether to discard the changes that were just persisted.
   */
  it('lands on the created version even while the page has the unsaved-changes guard armed', async () => {
    captureVersionPuts();
    useNavBlockerStore.getState().setBlockNav(true);
    const { router } = renderPipelinesRoute(<Probe readGraphDraft={() => liveGraph} />, '/pipelines/my/42/1', {
      withNavBlocker: true,
    });

    await userEvent.setup().click(await screen.findByTestId('saved'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/my/42/7'));
  });

  /**
   * The same disarm on the delete path, where being blocked is worse: the
   * navigation is an ESCAPE from a URL whose version no longer exists.
   * Red without it: pathname stays `/pipelines/my/42/1`.
   */
  it('escapes the deleted version even while the unsaved-changes guard is armed', async () => {
    useNavBlockerStore.getState().setBlockNav(true);
    const { router } = renderPipelinesRoute(<Probe readGraphDraft={() => undefined} />, '/pipelines/my/42/1', {
      withNavBlocker: true,
    });

    await userEvent.setup().click(await screen.findByTestId('deleted'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/my/42'));
  });

  /**
   * An ORDINARY version switch must still be guarded — the user really would
   * lose unsaved work. This is the discriminator that stops the two disarms
   * above from being written as one blanket disarm inside `goToVersion`.
   */
  it('still blocks an ordinary version switch while the guard is armed', async () => {
    useNavBlockerStore.getState().setBlockNav(true);
    const { router } = renderPipelinesRoute(<Probe readGraphDraft={() => undefined} />, '/pipelines/my/42/1', {
      withNavBlocker: true,
    });

    await userEvent.setup().click(await screen.findByTestId('select'));

    await waitFor(() => expect(screen.getByTestId('nav-blocker-dialog')).toBeInTheDocument());
    expect(router.state.location.pathname).toBe('/pipelines/my/42/1');
  });

  /**
   * "Save As Version" is a SECOND write path onto the same document. Gated
   * only on `!isReadOnly && activeVersion !== undefined`, it persisted exactly
   * the graph the Save veto had refused.
   */
  it('withholds Save As Version while the live graph is inadmissible, and only that button', async () => {
    renderPipelinesRoute(<Probe readGraphDraft={() => liveGraph} isGraphAdmissible={false} />, '/pipelines/my/42/1');

    expect(await screen.findByTestId('save-version-blocked')).toHaveTextContent('true');
    // The writer gate is untouched: `canSaveNewVersion` also governs "Delete
    // version" and "Set as default", and an inadmissible canvas must not take
    // away the delete that is sometimes the only way out of a bad version.
    expect(screen.getByTestId('can-save-version')).toHaveTextContent('true');
  });

  /**
   * And the carry itself re-asks, against the document actually about to be
   * written — for a graph that went inadmissible between the click and the
   * POST's response. The version exists either way, so the navigation still
   * happens; what must not happen is the PUT.
   */
  it('refuses to carry an inadmissible graph onto the created version, and says so', async () => {
    const puts = captureVersionPuts();
    const { router } = renderPipelinesRoute(<Probe readGraphDraft={() => inadmissibleGraph} />, '/pipelines/my/42/1');

    await userEvent.setup().click(await screen.findByTestId('saved'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/my/42/7'));
    expect(puts).toHaveLength(0);
    expect(screen.getByTestId('error')).toHaveTextContent('the runtime would refuse that graph');
  });

  /**
   * A refused delete used to leave the confirm dialog sitting open with no
   * message anywhere: the only failure channel was `onError`, and this page
   * did not supply it. `DeleteVersionDialog` now names the refusal inside the
   * dialog too (#147); this proves the banner still gets its copy.
   */
  it('routes a refused version delete into the version bar banner', async () => {
    renderPipelinesRoute(<Probe readGraphDraft={() => undefined} />, '/pipelines/my/42/1');

    await userEvent.setup().click(await screen.findByTestId('delete-failed'));

    expect(screen.getByTestId('error')).toHaveTextContent('Published version can not be updated/deleted.');
  });

  /**
   * The banner is scoped to the version it was raised on. It used to be
   * cleared only inside the successful-carry branch, so one transient 500
   * pinned it next to the dropdown for the rest of the page's life — this bar
   * stays mounted across every version navigation.
   */
  it('clears a stale version-write banner once the active version changes', async () => {
    renderPipelinesRoute(<Probe readGraphDraft={() => undefined} />, '/pipelines/my/42/1');
    const user = userEvent.setup();

    await user.click(await screen.findByTestId('delete-failed'));
    expect(screen.getByTestId('error')).toHaveTextContent('Published version can not be updated/deleted.');

    await user.click(screen.getByTestId('switch-active-version'));

    await waitFor(() => expect(screen.getByTestId('error')).toHaveTextContent('undefined'));
  });
});
