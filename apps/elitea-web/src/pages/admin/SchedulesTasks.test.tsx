/**
 * Rendering and behaviour tests for `pages/admin/SchedulesTasks.tsx`
 * (issue #1369).
 *
 * The page's writes switch platform routines off and change when they run, so
 * each test asserts one of:
 *
 *  - the REQUEST the control produced (a toggle that sends the wrong id, or
 *    smuggles a field, looks identical on screen to one that does not);
 *  - that a control the server would refuse is not offered as though it worked.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminSchedulesTasks } from './SchedulesTasks';
import { renderAdminRoute } from './__tests__/testRouter';
import type { AdminRoutineRow } from './api/adminSchedulesApi';

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

function routine(overrides: Partial<AdminRoutineRow> & { id: string }): AdminRoutineRow {
  return {
    owner: 'main',
    description: `${overrides.id} description`,
    default_schedule: '* * * * *',
    schedule_override: null,
    schedule: '* * * * *',
    min_interval_seconds: 60,
    enabled: true,
    last_run_at: null,
    last_outcome: null,
    next_run_at: '2026-08-01T10:05:00Z',
    updated_by: null,
    updated_at: '2026-08-01T09:00:00Z',
    ...overrides,
  };
}

const ROUTINES: AdminRoutineRow[] = [
  routine({
    id: 'index.schedule.scan.v1',
    schedule: '*/5 * * * *',
    schedule_override: '*/5 * * * *',
    last_run_at: '2026-08-01T10:00:00Z',
    last_outcome: 'ok',
  }),
  // Off on purpose: a routine an operator switched off must still be listed.
  routine({ id: 'scheduler.audit_retention', owner: 'scheduler', enabled: false, schedule: '1h', default_schedule: '1h', next_run_at: null }),
  routine({ id: 'scheduler.price_sync', owner: 'scheduler', schedule: '24h', default_schedule: '24h', last_run_at: '2026-08-01T08:00:00Z', last_outcome: 'error' }),
];

let listBody: () => Response = () => HttpResponse.json({ rows: ROUTINES, total: ROUTINES.length });

function useRoutineHandlers(): void {
  server.use(
    http.get('*/admin/platform_routines/administration', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return listBody();
    }),
    http.put('*/admin/platform_routines/administration/:id', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      return HttpResponse.json(ROUTINES[0]);
    }),
  );
}

function grantAdminUiPermissions(permissions: string[]): void {
  window.admin_ui_config = { permissions, vite_server_url: '/api/v2' };
}

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method === 'PUT');
}

async function waitForRoutines(): Promise<void> {
  await screen.findByText('index.schedule.scan.v1');
}

/** The routine id of each body row, in render order. */
function renderedIds(): string[] {
  const table = screen.getByRole('table');
  return within(table)
    .getAllByRole('row')
    .slice(1)
    .map((row) => within(row).getAllByRole('cell')[0]!.querySelector('span')?.textContent ?? '');
}

beforeEach(() => {
  recorded = [];
  listBody = () => HttpResponse.json({ rows: ROUTINES, total: ROUTINES.length });
  configureGeneratedClient({ baseUrl: '/api/v2' });
  grantAdminUiPermissions([
    'configuration.scheduling.schedules.view',
    'configuration.scheduling.schedules.edit',
  ]);
  useRoutineHandlers();
});

afterEach(() => {
  resetGeneratedClient();
  delete window.admin_ui_config;
});

describe('AdminSchedulesTasks - the listing', () => {
  it('renders every routine with its description and schedule', async () => {
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();

    expect(screen.getByText('index.schedule.scan.v1 description')).toBeInTheDocument();
    expect(screen.getByText('24h')).toBeInTheDocument();
    // A custom schedule says what it replaced; a default one says so too.
    expect(screen.getByText('Custom (default * * * * *)')).toBeInTheDocument();
    expect(screen.getAllByText('Default')).toHaveLength(2);
  });

  it('renders a switched-off routine with its switch off rather than hiding it', async () => {
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();

    expect(screen.getByRole('switch', { name: 'Schedule enabled: scheduler.audit_retention' })).not.toBeChecked();
    expect(screen.getByRole('switch', { name: 'Schedule enabled: index.schedule.scan.v1' })).toBeChecked();
    // An off routine has no next run to show.
    expect(screen.getByText('Off')).toBeInTheDocument();
  });

  it('shows a failed last run with its outcome', async () => {
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();
    expect(screen.getByText(/\(error\)$/)).toBeInTheDocument();
  });

  it('filters on the id and on the description', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();

    await user.type(screen.getByTestId('admin-schedules-search'), 'price_sync');

    await waitFor(() => expect(screen.queryByText('index.schedule.scan.v1')).not.toBeInTheDocument());
    expect(screen.getByText('scheduler.price_sync')).toBeInTheDocument();
  });

  it('sorts on a header click without asking the server again', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();
    const readsBefore = recorded.filter((entry) => entry.method === 'GET').length;

    await user.click(screen.getByRole('button', { name: 'Name' }));

    await waitFor(() => expect(renderedIds()[0]).toBe('scheduler.price_sync'));
    expect(recorded.filter((entry) => entry.method === 'GET')).toHaveLength(readsBefore);
  });

  it('sorts never-run routines LAST in both directions, not as the epoch', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();

    await user.click(screen.getByRole('button', { name: 'Last run' }));
    await waitFor(() => expect(renderedIds()[0]).toBe('scheduler.price_sync'));

    await user.click(screen.getByRole('button', { name: 'Last run' }));
    await waitFor(() => expect(renderedIds()[0]).toBe('index.schedule.scan.v1'));
    expect(renderedIds().at(-1)).toBe('scheduler.audit_retention');
  });

  it("surfaces the server's own explanation instead of a generic failure", async () => {
    listBody = () => HttpResponse.json({ error: 'failed to read the platform routines' }, { status: 500 });
    renderAdminRoute(<AdminSchedulesTasks />);

    const alert = await screen.findByTestId('admin-schedules-unavailable');
    expect(alert).toHaveTextContent('failed to read the platform routines');
  });

  it("quotes the server's explanation of a 403", async () => {
    listBody = () => HttpResponse.json({ error: 'permission denied' }, { status: 403 });
    renderAdminRoute(<AdminSchedulesTasks />);

    const alert = await screen.findByTestId('admin-schedules-unavailable');
    expect(alert).toHaveTextContent('permission denied');
  });
});

describe('AdminSchedulesTasks - the write', () => {
  it('sends only the flipped enabled flag, to the routine id in the path', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();

    await user.click(screen.getByRole('switch', { name: 'Schedule enabled: index.schedule.scan.v1' }));

    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(writes()[0]!.url).toMatch(/\/admin\/platform_routines\/administration\/index\.schedule\.scan\.v1$/);
    expect(writes()[0]!.body).toEqual({ enabled: false });
  });

  it('sends the edited schedule and nothing else', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();

    await user.click(screen.getByRole('button', { name: '*/5 * * * *' }));
    const field = await screen.findByRole('textbox', { name: 'Schedule (interval or cron)' });
    await user.clear(field);
    await user.type(field, '30m');
    await user.tab();

    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(writes()[0]!.body).toEqual({ schedule: '30m' });
  });

  it('sends a schedule committed with Enter once, although the closing editor also blurs', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();

    await user.click(screen.getByRole('button', { name: '*/5 * * * *' }));
    const field = await screen.findByRole('textbox', { name: 'Schedule (interval or cron)' });
    await user.clear(field);
    await user.type(field, '30m');
    // One act: the blur reaches the handler that still holds the draft.
    act(() => {
      fireEvent.keyDown(field, { key: 'Enter' });
      fireEvent.blur(field);
    });

    await waitFor(() => expect(writes()).toHaveLength(1));
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(writes()).toHaveLength(1);
  });

  it('sends an empty schedule to restore the default of an overridden routine', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();

    await user.click(screen.getByRole('button', { name: '*/5 * * * *' }));
    const field = await screen.findByRole('textbox', { name: 'Schedule (interval or cron)' });
    await user.clear(field);
    await user.tab();

    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(writes()[0]!.body).toEqual({ schedule: '' });
  });

  it('does not save a schedule the operator abandoned with Escape', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();

    await user.click(screen.getByRole('button', { name: '*/5 * * * *' }));
    const field = await screen.findByRole('textbox', { name: 'Schedule (interval or cron)' });
    await user.clear(field);
    await user.type(field, 'nonsense');
    await user.keyboard('{Escape}');

    await screen.findByRole('button', { name: '*/5 * * * *' });
    expect(writes()).toHaveLength(0);
  });

  it('does not save a schedule that was opened and left unchanged', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();

    await user.click(screen.getByRole('button', { name: '*/5 * * * *' }));
    await screen.findByRole('textbox', { name: 'Schedule (interval or cron)' });
    await user.tab();

    await screen.findByRole('button', { name: '*/5 * * * *' });
    expect(writes()).toHaveLength(0);
  });

  it("shows the server's own reason when a schedule is rejected", async () => {
    const user = userEvent.setup();
    server.use(
      http.put('*/admin/platform_routines/administration/:id', async ({ request }) => {
        recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
        return HttpResponse.json(
          { error: 'schedule fires more often than this routine allows: at least 1m0s between runs' },
          { status: 400 },
        );
      }),
    );
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();

    await user.click(screen.getByRole('button', { name: '*/5 * * * *' }));
    const field = await screen.findByRole('textbox', { name: 'Schedule (interval or cron)' });
    await user.clear(field);
    await user.type(field, '10s');
    await user.tab();

    const alert = await screen.findByTestId('admin-schedules-error');
    expect(alert).toHaveTextContent('fires more often than this routine allows');
    expect(writes()[0]!.body).toEqual({ schedule: '10s' });
  });
});

describe('AdminSchedulesTasks - authorisation is presentation only', () => {
  it('disables the switch and the schedule editor without the edit permission', async () => {
    grantAdminUiPermissions(['configuration.scheduling.schedules.view']);
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();

    expect(screen.getByRole('switch', { name: 'Schedule enabled: index.schedule.scan.v1' })).toBeDisabled();
    expect(screen.queryByRole('button', { name: '*/5 * * * *' })).not.toBeInTheDocument();
    expect(screen.getByText('*/5 * * * *')).toBeInTheDocument();
    expect(writes()).toHaveLength(0);
  });
});

describe('AdminSchedulesTasks - routines only', () => {
  it('renders no tab strip and requests nothing about tasks or history', async () => {
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForRoutines();

    expect(screen.queryByRole('tab')).not.toBeInTheDocument();
    expect(recorded.filter((entry) => /tasks|audit/.test(entry.url))).toHaveLength(0);
  });
});
