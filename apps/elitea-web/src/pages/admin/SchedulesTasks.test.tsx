/**
 * Rendering and behaviour tests for `pages/admin/SchedulesTasks.tsx` (unit A14).
 *
 * The bar these have to clear is not "the page renders". This page's one write
 * enables and disables PLATFORM jobs that then run unattended, so each test
 * asserts one of:
 *
 *  - the REQUEST the control produced (a toggle that sends the wrong id, or
 *    that smuggles a field, looks identical on screen to one that does not);
 *  - that a control the server would refuse is not offered as though it worked.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminSchedulesTasks } from './SchedulesTasks';
import { renderAdminRoute } from './__tests__/testRouter';

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

interface ScheduleFixture {
  readonly id: number;
  readonly name: string;
  readonly project_id: number | null;
  readonly cron: string;
  readonly active: boolean;
  readonly rpc_func: string;
  readonly rpc_kwargs: Record<string, unknown>;
  readonly last_run: string | null;
}

let recorded: RecordedRequest[] = [];

const SCHEDULES: ScheduleFixture[] = [
  {
    id: 11,
    name: 'index_scheduling',
    project_id: null,
    cron: '*/5 * * * *',
    active: true,
    rpc_func: 'indexer_scan',
    rpc_kwargs: {},
    last_run: '2026-08-01T10:00:00Z',
  },
  {
    // Inactive on purpose: the handler this page replaced hardcoded
    // `enabled: true`, so a disabled schedule could not be represented at all.
    id: 12,
    name: 'storage_used_space_check',
    project_id: null,
    cron: '0 3 * * *',
    active: false,
    rpc_func: 'storage_space_check',
    rpc_kwargs: {},
    last_run: null,
  },
  {
    id: 13,
    name: 'usage_monitor',
    project_id: null,
    cron: '0 * * * *',
    active: true,
    rpc_func: 'usage_collect',
    rpc_kwargs: { window: 60 },
    last_run: null,
  },
];

let listBody: () => Response = () =>
  HttpResponse.json({ rows: SCHEDULES, total: SCHEDULES.length });

function useScheduleHandlers(): void {
  server.use(
    http.get('*/scheduling/schedules/administration/0', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return listBody();
    }),
    http.put('*/scheduling/schedules/administration/0', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      return HttpResponse.json({ id: 11 });
    }),
    // The history drawer runs on the Audit Trail port's endpoint unchanged.
    http.get('*/elitea_core/audit/administration', ({ request }) => {
      recorded.push({ method: 'GET-audit', url: request.url, body: null });
      return HttpResponse.json({
        rows: [
          {
            id: 1,
            timestamp: '2026-08-01T10:00:00Z',
            event_type: 'schedule',
            action: 'Schedule: index_scheduling -> indexer_scan',
            duration_ms: 12.5,
            is_error: false,
            user_id: null,
            user_email: null,
            project_id: null,
            http_method: null,
            http_route: null,
            status_code: null,
            entity_name: null,
            tool_name: null,
            model_name: null,
            trace_id: null,
            span_id: null,
            parent_span_id: null,
          },
        ],
        total: 1,
      });
    }),
  );
}

/** The permission list the Go adminui handler injects for a valid session. */
function grantAdminUiPermissions(permissions: string[]): void {
  window.admin_ui_config = { permissions, vite_server_url: '/api/v2' };
}

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method === 'PUT');
}

/**
 * The schedule name is a `Link component="button"`, so its accessible role is
 * `button` and not `link` — asserting on `link` passes vacuously nowhere and
 * fails everywhere, which is how this was caught.
 */
async function waitForSchedules(): Promise<void> {
  await screen.findByRole('button', { name: 'index_scheduling' });
}

/** The schedule NAME cell of each body row, in render order. */
function renderedScheduleNames(): string[] {
  const table = screen.getByRole('table');
  return within(table)
    .getAllByRole('row')
    .slice(1)
    .map((row) => within(row).getAllByRole('cell')[0]!.textContent ?? '');
}

beforeEach(() => {
  recorded = [];
  listBody = () => HttpResponse.json({ rows: SCHEDULES, total: SCHEDULES.length });
  configureGeneratedClient({ baseUrl: '/api/v2' });
  grantAdminUiPermissions([
    'configuration.scheduling.schedules.view',
    'configuration.scheduling.schedules.edit',
  ]);
  useScheduleHandlers();
});

afterEach(() => {
  resetGeneratedClient();
  delete window.admin_ui_config;
});

describe('AdminSchedulesTasks — the listing', () => {
  it('renders every schedule with the function it invokes and its last run', async () => {
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();

    // `rpc_func` is on screen because it is the only thing that says WHAT a
    // schedule does; a row without it is a cron with no meaning.
    expect(screen.getByText('indexer_scan')).toBeInTheDocument();
    expect(screen.getByText('storage_space_check')).toBeInTheDocument();
    expect(screen.getByText('usage_collect')).toBeInTheDocument();

    // A schedule that has never run says so rather than showing an em dash or
    // the epoch.
    expect(screen.getAllByText('Never')).toHaveLength(2);
  });

  it('renders an inactive schedule with its switch off rather than hiding it', async () => {
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();

    const off = screen.getByRole('switch', { name: 'Schedule enabled: storage_used_space_check' });
    expect(off).not.toBeChecked();
    const on = screen.getByRole('switch', { name: 'Schedule enabled: index_scheduling' });
    expect(on).toBeChecked();
  });

  it('filters on the name and on the function, not on the name alone', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();

    // `usage_collect` is the FUNCTION of `usage_monitor`. Searching for it must
    // find that row: an operator chasing a runaway job knows the function name
    // from the logs, not the schedule's label.
    await user.type(screen.getByTestId('admin-schedules-search'), 'usage_collect');

    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'index_scheduling' })).not.toBeInTheDocument(),
    );
    expect(screen.getByRole('button', { name: 'usage_monitor' })).toBeInTheDocument();
  });

  it('sorts on a header click without asking the server again', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();
    const readsBefore = recorded.filter((entry) => entry.method === 'GET').length;

    await user.click(screen.getByRole('button', { name: 'Name' }));

    // Descending by name: usage_monitor first.
    await waitFor(() => expect(renderedScheduleNames()[0]).toBe('usage_monitor'));
    // The endpoint is unpaginated and the whole table is already here, so a
    // sort that refetched would be asking for rows it holds.
    expect(recorded.filter((entry) => entry.method === 'GET')).toHaveLength(readsBefore);
  });

  it('sorts never-run schedules LAST in both directions, not as the epoch', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();

    // "Never" is not "a very long time ago". Sorting it as the epoch buries the
    // rows an operator most wants to notice at whichever end they are looking
    // away from, so it is pinned to the bottom of BOTH orders — which is also
    // why one direction alone cannot prove the comparator right.
    await user.click(screen.getByRole('button', { name: 'Last run' }));
    await waitFor(() => expect(renderedScheduleNames()[0]).toBe('index_scheduling'));

    await user.click(screen.getByRole('button', { name: 'Last run' }));
    await waitFor(() => expect(renderedScheduleNames().at(-1)).toBe('index_scheduling'));
    // The two never-run rows hold the other two places in both orders.
    expect(renderedScheduleNames().slice(0, 2).sort()).toEqual([
      'storage_used_space_check',
      'usage_monitor',
    ]);
  });

  it("surfaces the server's own explanation instead of a generic failure", async () => {
    listBody = () => HttpResponse.json({ error: 'failed to read schedules' }, { status: 500 });
    renderAdminRoute(<AdminSchedulesTasks />);

    const alert = await screen.findByTestId('admin-schedules-unavailable');
    expect(alert).toHaveTextContent('failed to read schedules');
  });

  it("quotes the server's explanation of a 403 rather than a generic failure", async () => {
    // This pin fired, and the change it caught is the intended one (issue 93):
    // `shared/api/http.ts` no longer escalates a 403 into the single-flight
    // re-auth path, so a refusal now arrives as `kind: 'http'` WITH its body
    // instead of as a body-less `kind: 'auth'`. Re-pinned the other way round —
    // an operator refused this page is told why.
    listBody = () => HttpResponse.json({ error: 'permission denied' }, { status: 403 });
    renderAdminRoute(<AdminSchedulesTasks />);

    const alert = await screen.findByTestId('admin-schedules-unavailable');
    expect(alert).toHaveTextContent('permission denied');
  });
});

describe('AdminSchedulesTasks — the write', () => {
  it('sends only the id and the flipped active flag when the switch is toggled', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();

    await user.click(screen.getByRole('switch', { name: 'Schedule enabled: index_scheduling' }));

    await waitFor(() => expect(writes()).toHaveLength(1));
    // The body is asserted field by field, not just "a PUT happened": a request
    // carrying `rpc_func` would be refused by the server, and one carrying the
    // wrong id would disable a different platform job.
    expect(writes()[0]!.body).toEqual({ id: 11, active: false });
  });

  it('sends the edited cron and nothing else', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();

    await user.click(screen.getByRole('button', { name: '*/5 * * * *' }));
    const field = await screen.findByRole('textbox', { name: 'Cron (5 fields)' });
    await user.clear(field);
    await user.type(field, '*/15 * * * *');
    await user.tab();

    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(writes()[0]!.body).toEqual({ id: 11, cron: '*/15 * * * *' });
  });

  it('does not save a cron the operator abandoned with Escape', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();

    await user.click(screen.getByRole('button', { name: '*/5 * * * *' }));
    const field = await screen.findByRole('textbox', { name: 'Cron (5 fields)' });
    await user.clear(field);
    await user.type(field, 'nonsense');
    await user.keyboard('{Escape}');

    await screen.findByRole('button', { name: '*/5 * * * *' });
    expect(writes()).toHaveLength(0);
  });

  it('does not save a cron that was opened and left unchanged', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();

    await user.click(screen.getByRole('button', { name: '*/5 * * * *' }));
    await screen.findByRole('textbox', { name: 'Cron (5 fields)' });
    await user.tab();

    // A no-op save would report success for an edit that did not happen, and
    // would stamp the audit trail with a change nobody made.
    await screen.findByRole('button', { name: '*/5 * * * *' });
    expect(writes()).toHaveLength(0);
  });

  it("shows the server's own reason when a cron is rejected", async () => {
    const user = userEvent.setup();
    server.use(
      http.put('*/scheduling/schedules/administration/0', async ({ request }) => {
        recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
        return HttpResponse.json(
          { error: 'cron expression is invalid: five fields are required' },
          { status: 400 },
        );
      }),
    );
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();

    await user.click(screen.getByRole('button', { name: '*/5 * * * *' }));
    const field = await screen.findByRole('textbox', { name: 'Cron (5 fields)' });
    await user.clear(field);
    await user.type(field, '@daily');
    await user.tab();

    // `@daily` is a descriptor robfig accepts only WITH `cron.Descriptor`, and
    // the scheduler's parser is built without it — so the server refuses it and
    // the operator has to be told which of their inputs was wrong.
    const alert = await screen.findByTestId('admin-schedules-error');
    expect(alert).toHaveTextContent('cron expression is invalid: five fields are required');
    expect(writes()[0]!.body).toEqual({ id: 11, cron: '@daily' });
  });
});

describe('AdminSchedulesTasks — authorisation is presentation only', () => {
  it('disables the switch and the cron editor without the edit permission', async () => {
    const user = userEvent.setup();
    grantAdminUiPermissions(['configuration.scheduling.schedules.view']);
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();

    expect(screen.getByRole('switch', { name: 'Schedule enabled: index_scheduling' })).toBeDisabled();
    // The cron renders as text, so there is no button to open an editor with.
    expect(screen.queryByRole('button', { name: '*/5 * * * *' })).not.toBeInTheDocument();
    expect(screen.getByText('*/5 * * * *')).toBeInTheDocument();

    // The name link still works: read access is read access.
    await user.click(screen.getByRole('button', { name: 'index_scheduling' }));
    await screen.findByText('Execution history');
    expect(writes()).toHaveLength(0);
  });
});

describe('AdminSchedulesTasks — schedules only', () => {
  // The reference page's Tasks and Active Tasks tabs read a plugin runtime's
  // task registry this platform does not have; Admin › Tasks answers that
  // question from the platform's own tables. This page must not grow them back
  // as tabs that render nothing.
  it('renders no tab strip and requests nothing about tasks', async () => {
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();

    expect(screen.queryByRole('tab')).not.toBeInTheDocument();
    expect(screen.getByTestId('admin-schedules-search')).toBeInTheDocument();
    expect(recorded.filter((entry) => entry.url.includes('tasks'))).toHaveLength(0);
  });
});

describe('AdminSchedulesTasks — execution history', () => {
  it('queries schedule audit events for the clicked schedule', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();

    await user.click(screen.getByRole('button', { name: 'index_scheduling' }));

    await waitFor(() =>
      expect(recorded.some((entry) => entry.method === 'GET-audit')).toBe(true),
    );
    const auditRequest = recorded.find((entry) => entry.method === 'GET-audit')!;
    const query = new URL(auditRequest.url).searchParams;
    // Scoped to schedule executions, and to THIS schedule's span name.
    expect(query.get('event_type')).toBe('schedule');
    expect(query.get('search')).toBe('Schedule: index_scheduling');

    const drawer = await screen.findByText('Execution history');
    expect(drawer).toBeInTheDocument();
    expect(await screen.findByText('Succeeded')).toBeInTheDocument();
  });

  it('does not query the audit trail before a schedule is opened', () => {
    renderAdminRoute(<AdminSchedulesTasks />);
    expect(recorded.filter((entry) => entry.method === 'GET-audit')).toHaveLength(0);
  });

  it('says the period was empty rather than showing nothing at all', async () => {
    const user = userEvent.setup();
    server.use(
      http.get('*/elitea_core/audit/administration', () => HttpResponse.json({ rows: [], total: 0 })),
    );
    renderAdminRoute(<AdminSchedulesTasks />);
    await waitForSchedules();

    await user.click(screen.getByRole('button', { name: 'usage_monitor' }));

    const empty = await screen.findByTestId('admin-schedule-history-empty');
    expect(within(empty).getByText(/No executions recorded/)).toBeInTheDocument();
  });
});
