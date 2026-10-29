/**
 * The agent's release history (ADR-0040 decision 2, #1492): releases with the
 * current one marked, a release's changes, rollback with a reason, and the
 * open version's draft revisions.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import type { ApplicationRelease, ApplicationReleaseDetail, VersionRevisionList } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { ReleaseHistoryDialog } from '../ui/ReleaseHistoryDialog';
import { renderWithProviders } from './testUtils';

const release = (id: number, current: boolean, name: string): ApplicationRelease => ({
  id,
  application_id: 31,
  application_version_id: 100 + id,
  source_version_id: 90,
  name,
  definition_digest: `${String(id).repeat(12)}${'a'.repeat(52)}`.slice(0, 64),
  state: 'published',
  is_published_release: current,
  published_by: 4,
  created_at: '2026-10-28T09:15:02Z',
  withdrawn_at: null,
  withdrawn_by: null,
});

const DETAIL: ApplicationReleaseDetail = {
  ...release(2, true, 'v2'),
  definition: {},
  toolkit_pins: [],
  previous_release_id: 1,
  changes: [{ op: 'replace', path: '/instructions', before: 'Be brief.', after: 'Be thorough.' }],
};

const REVISIONS: VersionRevisionList = {
  total: 2,
  rows: [
    {
      id: 12,
      revision: 2,
      kind: 'save',
      actor_id: 4,
      created_at: '2026-10-28T10:00:00Z',
      snapshot: {},
      diff: [{ op: 'replace', path: '/llm_settings/temperature', before: 0.2, after: 0.7 }],
    },
    { id: 11, revision: 1, kind: 'baseline', actor_id: 4, created_at: '2026-10-28T09:00:00Z', snapshot: {}, diff: [] },
  ],
};

let rollbacks: unknown[];

beforeEach(() => {
  rollbacks = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/application_releases/prompt_lib/7/31', () =>
      HttpResponse.json({ total: 2, rows: [release(2, true, 'v2'), release(1, false, 'v1')] }),
    ),
    http.get('*/application_releases/prompt_lib/7/31/2', () => HttpResponse.json(DETAIL)),
    http.post('*/application_releases/prompt_lib/7/31/1/rollback', async ({ request }) => {
      rollbacks.push(await request.json());
      return HttpResponse.json({ from: release(2, false, 'v2'), to: release(1, true, 'v1'), reason: 'regression' });
    }),
    http.get('*/version_revisions/prompt_lib/7/31/90', () => HttpResponse.json(REVISIONS)),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

function renderDialog() {
  return renderWithProviders(
    <ReleaseHistoryDialog
      open
      projectId="7"
      applicationId={31}
      versionId={90}
      onClose={() => undefined}
    />,
  );
}

describe('ReleaseHistoryDialog', () => {
  it('marks the current release and offers rollback only to an earlier one', async () => {
    renderDialog();

    const current = await screen.findByTestId('release-row-2');
    expect(within(current).getByText('Current')).toBeInTheDocument();
    expect(screen.queryByTestId('release-rollback-2')).not.toBeInTheDocument();
    expect(screen.getByTestId('release-rollback-1')).toBeInTheDocument();
  });

  it("shows a release's changes against the release before it", async () => {
    const user = userEvent.setup();
    renderDialog();

    const current = await screen.findByTestId('release-row-2');
    await user.click(within(current).getByRole('button', { name: 'Changes' }));

    const changes = await screen.findByTestId('release-changes');
    expect(within(changes).getByText('Release #2 compared with release #1')).toBeInTheDocument();
    expect(within(changes).getByText('replace /instructions: Be thorough.')).toBeInTheDocument();
  });

  it('rolls back with the reason the audit trail keeps', async () => {
    const user = userEvent.setup();
    renderDialog();

    await user.click(await screen.findByTestId('release-rollback-1'));
    await user.type(screen.getByLabelText('Reason (recorded in the audit event)'), 'regression');
    await user.click(screen.getByTestId('release-rollback-confirm'));

    await waitFor(() => expect(rollbacks).toEqual([{ reason: 'regression' }]));
  });

  it("lists the open version's revisions with what each save changed", async () => {
    const user = userEvent.setup();
    renderDialog();

    await user.click(await screen.findByRole('tab', { name: 'Revisions of this version' }));

    const list = await screen.findByTestId('revision-list');
    expect(within(list).getByText('replace /llm_settings/temperature: 0.7')).toBeInTheDocument();
    expect(within(list).getByText('The version as it was before its first recorded save.')).toBeInTheDocument();
  });

  it('says the agent is not saved yet instead of loading forever when there is no version', async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <ReleaseHistoryDialog
        open
        projectId="7"
        applicationId={31}
        versionId={undefined}
        onClose={() => undefined}
      />,
    );

    await user.click(await screen.findByRole('tab', { name: 'Revisions of this version' }));

    expect(
      await screen.findByText('Save the agent first; its history starts with its first saved version.'),
    ).toBeInTheDocument();
  });
});
