/**
 * Admin › Kill switch (ADR-0041 decision 1): the active suspensions, Resume,
 * and Suspend with the platform stop behind the typed word STOP.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import type { ExecutionSuspensionRow } from '@/features/kill-switch';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { MAX_SUSPENSION_ID, PLATFORM_STOP_CONFIRMATION, suspensionBody } from './AdminSuspendDialog';
import { AdminKillSwitch } from './KillSwitch';
import { renderAdminRoute } from './__tests__/testRouter';

const ROW: ExecutionSuspensionRow = {
  id: 12,
  scope: 'agent',
  project_id: 7,
  application_id: 31,
  toolkit_type: null,
  reason: 'The agent sends repeated messages',
  actor_id: 4,
  source: 'manual',
  resume_requires: 'any_admin',
  created_at: '2026-10-28T09:15:02Z',
  cancelled_runs: 2,
  active: true,
  resumed_at: null,
  resumed_by: null,
  resume_reason: null,
};

let rows: ExecutionSuspensionRow[];
let creates: unknown[];
let resumes: string[];

beforeEach(() => {
  rows = [ROW];
  creates = [];
  resumes = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/execution_suspensions', () => HttpResponse.json({ suspensions: rows })),
    http.post('*/admin/execution_suspensions', async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      creates.push(body);
      const created = { ...ROW, id: 13, scope: String(body['scope']), project_id: null, application_id: null };
      rows = [created, ...rows];
      return HttpResponse.json(created, { status: 201 });
    }),
    http.post('*/admin/execution_suspensions/:id', ({ params }) => {
      const id = String(params['id']);
      resumes.push(id);
      if (id === '12:resume' && rows[0]?.resume_requires === 'two_person') {
        return HttpResponse.json({ error: 'no', code: 'resume_forbidden' }, { status: 403 });
      }
      rows = rows.filter((row) => `${row.id}:resume` !== id);
      return HttpResponse.json({ ...ROW, active: false });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AdminKillSwitch', () => {
  it('lists the active suspensions and resumes one', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminKillSwitch refetchInterval={false} />);

    const row = await screen.findByTestId('kill-switch-row-12');
    expect(row).toHaveTextContent('Project 7, agent 31');
    expect(row).toHaveTextContent('Any administrator');
    await user.click(within(row).getByRole('button', { name: 'Resume' }));

    await waitFor(() => {
      expect(resumes).toEqual(['12:resume']);
    });
    expect(await screen.findByText('No suspension is active. Every run is admitted.')).toBeInTheDocument();
  });

  it('says why a resume is refused', async () => {
    rows = [{ ...ROW, resume_requires: 'two_person' }];
    const user = userEvent.setup();
    renderAdminRoute(<AdminKillSwitch refetchInterval={false} />);

    await user.click(within(await screen.findByTestId('kill-switch-row-12')).getByRole('button', { name: 'Resume' }));
    expect(await screen.findByText(/does not let you resume it/)).toBeInTheDocument();
  });
});

describe('suspensionBody', () => {
  const base = {
    projectId: '',
    applicationId: '',
    toolkitType: '',
    reason: 'incident',
    resumeRequires: 'two_person',
    confirm: '',
  };

  it('holds the platform stop until STOP is typed', () => {
    expect(suspensionBody({ ...base, scope: 'platform', confirm: 'stop' })).toBeUndefined();
    expect(suspensionBody({ ...base, scope: 'platform', confirm: PLATFORM_STOP_CONFIRMATION })).toEqual({
      scope: 'platform',
      reason: 'incident',
      resume_requires: 'two_person',
      confirm: 'STOP',
    });
  });

  it('needs the key of each scope and a reason', () => {
    expect(suspensionBody({ ...base, scope: 'agent', projectId: '7' })).toBeUndefined();
    expect(suspensionBody({ ...base, scope: 'agent', projectId: '7', applicationId: '31' })).toMatchObject({
      project_id: 7,
      application_id: 31,
    });
    expect(suspensionBody({ ...base, scope: 'project', projectId: '-1' })).toBeUndefined();
    // Ids past a PostgreSQL integer are refused here and by the API (400).
    expect(suspensionBody({ ...base, scope: 'project', projectId: String(MAX_SUSPENSION_ID + 1) })).toBeUndefined();
    expect(suspensionBody({ ...base, scope: 'project', projectId: String(MAX_SUSPENSION_ID) })).toMatchObject({
      project_id: MAX_SUSPENSION_ID,
    });
    expect(suspensionBody({ ...base, scope: 'toolkit_type', toolkitType: ' GitHub ' })).toMatchObject({
      toolkit_type: 'github',
    });
    expect(suspensionBody({ ...base, scope: 'project', projectId: '7', reason: '  ' })).toBeUndefined();
  });

  it('says what each scope does to the runs in flight', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminKillSwitch refetchInterval={false} />);
    await screen.findByTestId('kill-switch-row-12');

    await user.click(screen.getByTestId('kill-switch-open'));
    expect(screen.getByTestId('kill-switch-effect')).toHaveTextContent('its runs in flight are cancelled');
    await user.click(screen.getByRole('combobox', { name: 'Scope' }));
    await user.click(await screen.findByRole('option', { name: 'Toolkit type' }));
    // A toolkit type suspension cancels nothing: the copy must not claim it does.
    const effect = screen.getByTestId('kill-switch-effect');
    expect(effect).toHaveTextContent('runs already in flight keep the toolkit until they end');
    expect(effect).not.toHaveTextContent('cancelled');
  });

  it('sends the platform stop through the dialog only after STOP', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminKillSwitch refetchInterval={false} />);
    await screen.findByTestId('kill-switch-row-12');

    await user.click(screen.getByTestId('kill-switch-open'));
    await user.click(screen.getByRole('combobox', { name: 'Scope' }));
    await user.click(await screen.findByRole('option', { name: 'Platform' }));
    await user.type(screen.getByRole('textbox', { name: 'Reason' }), 'incident');
    expect(screen.getByTestId('kill-switch-submit')).toBeDisabled();
    await user.type(screen.getByTestId('kill-switch-confirm'), 'STOP');
    await user.click(screen.getByTestId('kill-switch-submit'));

    await waitFor(() => {
      expect(creates).toEqual([{ scope: 'platform', reason: 'incident', resume_requires: 'any_admin', confirm: 'STOP' }]);
    });
  });
});
