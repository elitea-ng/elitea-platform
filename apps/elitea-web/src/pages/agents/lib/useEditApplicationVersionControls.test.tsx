import { QueryClient } from '@tanstack/react-query';
import { waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useForm } from 'react-hook-form';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import type { ApplicationCreationInput } from '@/entities/application-form';
import { getGetApplicationQueryKey } from '@/shared/api/generated/applications/applications';
import type { ApplicationVersionDetail, ApplicationVersionSummary } from '@/shared/api/generated/model';
import { useNavBlockerStore } from '@/widgets/app-shell';

import { renderAgentsRoute } from '../__tests__/testRouter';

import { useEditApplicationVersionControls } from './useEditApplicationVersionControls';
import {
  useEditApplicationVersionFields,
  type EditApplicationVersionFields,
} from './useEditApplicationVersionFields';

/**
 * Driven through the unit's REAL router fixture rather than a mocked
 * `useNavigate` — R-M1 (`elitea/no-vi-mock`) allows only the MSW network
 * boundary and the socket double to be substituted, and a mocked navigate
 * would in any case only prove the hook called a function, not that the
 * route it names resolves. Asserting `router.state.location.pathname` proves
 * the latter.
 */
const versions: readonly ApplicationVersionSummary[] = [
  // Ids arrive from the API as strings ("numeric id serialized as string").
  { id: '1', name: 'base', status: 'draft', agent_type: 'classic', created_at: '2026-01-01T00:00:00Z' },
  { id: '2', name: 'v1', status: 'draft', agent_type: 'classic', created_at: '2026-01-02T00:00:00Z' },
];

const activeVersion = {
  id: '1',
  application_id: '42',
  name: 'base',
  status: 'draft',
  agent_type: 'classic',
  instructions: 'be helpful',
  welcome_message: 'hi',
  variables: [{ name: 'k', value: 'v' }],
} as unknown as ApplicationVersionDetail;

interface ProbeProps {
  readonly starters: readonly string[];
  readonly isReadOnly: boolean;
  readonly isFetching: boolean;
  readonly applicationId: number | undefined;
  readonly version: ApplicationVersionDetail | undefined;
  /** Overrides the seeded fields, to stand in for edits the user has typed but not saved. */
  readonly edits?: EditApplicationVersionFields | undefined;
}

function Probe({ starters, isReadOnly, isFetching, applicationId, version, edits }: ProbeProps) {
  const form = useForm<ApplicationCreationInput>({
    values: { name: 'a', description: 'b', version_details: { conversation_starters: [...starters] } },
  });
  const versionFields = useEditApplicationVersionFields(version);
  const state = useEditApplicationVersionControls({
    projectId: '9',
    applicationId,
    tab: 'my',
    versions,
    activeVersion: version,
    control: form.control,
    versionFields: edits ?? versionFields.fields,
    isReadOnly,
    isFetching,
  });
  return (
    <div>
      <span data-testid="options">{JSON.stringify(state.versionOptions)}</span>
      <span data-testid="body">{JSON.stringify(state.versionBody)}</span>
      <span data-testid="active">{String(state.activeVersionId)}</span>
      <span data-testid="can-save">{String(state.canSaveNewVersion)}</span>
      <span data-testid="show">{String(state.showVersionControls)}</span>
      <span data-testid="id-text">{`[${state.applicationIdText}]`}</span>
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

function renderProbe(overrides: Partial<ProbeProps> = {}, queryClient?: QueryClient, withNavBlocker = false, initialPath = '/agents/my/42') {
  const props: ProbeProps = {
    starters: ['start here'],
    isReadOnly: false,
    isFetching: false,
    applicationId: 42,
    version: activeVersion,
    ...overrides,
  };
  return renderAgentsRoute(<Probe {...props} />, initialPath, {
    projectId: '9',
    withNavBlocker,
    ...(queryClient === undefined ? {} : { queryClient }),
  });
}

let queryClient: QueryClient;

beforeEach(() => {
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  useNavBlockerStore.getState().setBlockNav(false);
  return () => useNavBlockerStore.getState().setBlockNav(false);
});

describe('useEditApplicationVersionControls', () => {
  it("narrows the API's string version ids to the numbers the selector compares against", async () => {
    const { findByTestId, getByTestId } = renderProbe();
    expect(JSON.parse((await findByTestId('options')).textContent ?? '')).toEqual([
      { id: 1, name: 'base', created_at: '2026-01-01T00:00:00Z', status: 'draft', is_default: false },
      { id: 2, name: 'v1', created_at: '2026-01-02T00:00:00Z', status: 'draft', is_default: false },
    ]);
    expect(getByTestId('active').textContent).toBe('1');
  });

  it('switches version by NAVIGATING to the :version route', async () => {
    const { findByTestId, router } = renderProbe();

    await userEvent.click(await findByTestId('select'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/my/42/2'));
  });

  it('clones the LIVE form starters onto a new version, not the saved ones', async () => {
    const { findByTestId } = renderProbe({ starters: ['typed but unsaved'] });
    expect(JSON.parse((await findByTestId('body')).textContent ?? '')).toEqual({
      agent_type: 'classic',
      instructions: 'be helpful',
      welcome_message: 'hi',
      conversation_starters: ['typed but unsaved'],
      variables: [{ name: 'k', value: 'v' }],
      // The create path DOES read `meta` (`versionFromBody`,
      // `applications/handler.go:504`), so the clone carries it — otherwise a
      // Save-As-Version resets `step_limit` and drops `internal_tools`.
      // #898 — `notes` travels in the same blob; "" is the value an editor
      // with an empty Notes box sends.
      meta: { internal_tools: [], notes: '' },
    });
  });

  it('invalidates the application detail and navigates onto a newly created version', async () => {
    const invalidate = vi.spyOn(queryClient, 'invalidateQueries');
    const { findByTestId, router } = renderProbe({}, queryClient);

    await userEvent.click(await findByTestId('saved'));

    expect(invalidate).toHaveBeenCalledWith({ queryKey: getGetApplicationQueryKey('9', 42) });
    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/my/42/7'));
  });

  it('withholds "Save As Version" from a read-only viewer', async () => {
    const { findByTestId } = renderProbe({ isReadOnly: true });
    expect((await findByTestId('can-save')).textContent).toBe('false');
  });

  it('offers "Save As Version" to an owner viewing a resolved version', async () => {
    const { findByTestId } = renderProbe();
    expect((await findByTestId('can-save')).textContent).toBe('true');
  });

  it('withholds "Save As Version" until an active version has resolved', async () => {
    const { findByTestId, getByTestId } = renderProbe({ version: undefined });
    expect((await findByTestId('can-save')).textContent).toBe('false');
    expect(JSON.parse(getByTestId('body').textContent ?? '')).toEqual({});
  });

  it('hides the whole bar while the detail is still in flight', async () => {
    const { findByTestId } = renderProbe({ isFetching: true });
    expect((await findByTestId('show')).textContent).toBe('false');
  });

  it('hides the bar and never navigates when there is no agent id', async () => {
    const { findByTestId, getByTestId, router } = renderProbe({ applicationId: undefined });
    expect((await findByTestId('show')).textContent).toBe('false');
    expect(getByTestId('id-text').textContent).toBe('[]');

    await userEvent.click(getByTestId('select'));
    expect(router.state.location.pathname).toBe('/agents/my/42');
  });
  /**
   * #133 — `EditApplication` arms the app-wide unsaved-changes guard off its
   * own dirty state, and `NavBlockerDialog`'s `shouldBlockFn` blocks ANY
   * pathname change while it is raised, including the two this hook owns.
   * The fixture mounts the real dialog (`withNavBlocker`) because a router
   * without it cannot fail — that absence is why the unit suite never saw it.
   *
   * Without `disarmUnsavedChangesNavBlocker()` the user got a modal asking
   * whether to discard the changes that had just been persisted, and Cancel
   * left the URL on the old version while the new one silently held the work.
   */
  it('lands on the created version even while the page has the unsaved-changes guard armed', async () => {
    useNavBlockerStore.getState().setBlockNav(true);
    const { findByTestId, router } = renderProbe({}, undefined, true);

    await userEvent.click(await findByTestId('saved'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/my/42/7'));
  });

  /** Same disarm on the escape path, where a block strands the user on a version that no longer exists. */
  it('escapes the deleted version even while the unsaved-changes guard is armed', async () => {
    useNavBlockerStore.getState().setBlockNav(true);
    // Opened ON the version, which is the only case where the escape is a
    // real pathname CHANGE — `shouldBlockFn` ignores same-pathname
    // navigations, so starting at `/agents/my/42` cannot fail either way.
    const { findByTestId, router } = renderProbe({}, undefined, true, '/agents/my/42/1');

    await userEvent.click(await findByTestId('deleted'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/my/42'));
  });

  /** An ORDINARY version switch must still be guarded — the user really would lose unsaved work. */
  it('still blocks an ordinary version switch while the guard is armed', async () => {
    useNavBlockerStore.getState().setBlockNav(true);
    const { findByTestId, getByTestId, router } = renderProbe({}, undefined, true);

    await userEvent.click(await findByTestId('select'));

    await waitFor(() => expect(getByTestId('nav-blocker-dialog')).toBeInTheDocument());
    expect(router.state.location.pathname).toBe('/agents/my/42');
  });
});
