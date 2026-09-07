/**
 * Admin › Tasks, through the COMPOSITION ROOT.
 *
 * Every test renders `<AdminTasks/>` — the component the router mounts — over a
 * real MSW server. `AdminTasksTable` renders whatever rows it is handed, so a
 * test of the table alone stays green when nothing fetches, when the filters
 * reach no query string, and when the cancel button calls no endpoint. That is
 * the #597 class: both halves correct, the wiring the bug.
 *
 * The poll is disabled through the page's own `refetchInterval` PROP, not by
 * mocking the hook. A 10-second timer inside jsdom either makes the suite slow
 * or makes it flaky; substituting the hook would be worse, because a test that
 * replaces it stops proving the page is wired to it (R-M1).
 */
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderAdminRoute } from './__tests__/testRouter';
import { AdminTasks } from './Tasks';

const BASE = '/api/v2';
const JOBS_URL = `${BASE}/admin/background_jobs/administration`;

function runtimeJob(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    task_id: 'exec-1',
    kind: 'index',
    name: 'index.ingest.v1',
    status: 'RUNNING',
    started_at: '2026-09-07T10:00:00Z',
    finished_at: null,
    project_id: 7,
    user: '42',
    cancellable: true,
    ...overrides,
  };
}

/** Records every listing request so the filter assertions read the real query. */
function seedListing(rows: unknown[], seen?: string[], truncated = false): void {
  server.use(
    http.get(JOBS_URL, ({ request }) => {
      seen?.push(new URL(request.url).search);
      return HttpResponse.json({ total: rows.length, rows, truncated });
    }),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AdminTasks', () => {
  it('renders the jobs the server sends', async () => {
    seedListing([runtimeJob()]);
    renderAdminRoute(<AdminTasks refetchInterval={false} />);

    const row = await screen.findByTestId('admin-task-exec-1');
    expect(within(row).getByText('index.ingest.v1')).toBeInTheDocument();
    expect(within(row).getByText('RUNNING')).toBeInTheDocument();
    expect(await screen.findByTestId('admin-tasks-count')).toHaveTextContent('1');
  });

  /**
   * The nav journey (`e2e/journeys/admin/admin.navigation.spec.ts`, J37b) proves
   * a click LANDED by reading the destination's own heading, because a link to a
   * deleted route moves the address bar and renders nothing. Every other admin
   * page states its title with `variant="h5"`; this one uses `headingMedium`,
   * which is a real heading only because `shared/brand/mui-overrides/MuiTypography`
   * maps it to `h2`. Drop that mapping and the page still LOOKS right while the
   * journey stops being able to see it, so the landmark is asserted here too.
   */
  it('states its title as a real heading, the landmark the nav journey reads', async () => {
    seedListing([]);
    renderAdminRoute(<AdminTasks refetchInterval={false} />);
    expect(await screen.findByRole('heading', { name: 'Tasks', level: 2 })).toBeInTheDocument();
  });

  it('says the filters match nothing rather than rendering an empty table', async () => {
    seedListing([]);
    renderAdminRoute(<AdminTasks refetchInterval={false} />);
    expect(await screen.findByTestId('admin-tasks-empty')).toBeInTheDocument();
  });

  // The server filters and pages, so the filter must reach the QUERY STRING. A
  // client-side filter over one page would silently narrow a subset.
  it('sends the kind filter to the server', async () => {
    const seen: string[] = [];
    seedListing([runtimeJob()], seen);
    renderAdminRoute(<AdminTasks refetchInterval={false} />);
    await screen.findByTestId('admin-task-exec-1');

    await userEvent.click(screen.getByLabelText('Kind'));
    await userEvent.click(await screen.findByRole('option', { name: 'agent' }));

    await waitFor(() => {
      expect(seen.some((search) => search.includes('kind=agent'))).toBe(true);
    });
  });

  it('sends the status filter to the server', async () => {
    const seen: string[] = [];
    seedListing([runtimeJob()], seen);
    renderAdminRoute(<AdminTasks refetchInterval={false} />);
    await screen.findByTestId('admin-task-exec-1');

    await userEvent.click(screen.getByLabelText('Status'));
    await userEvent.click(await screen.findByRole('option', { name: 'FAILED' }));

    await waitFor(() => {
      expect(seen.some((search) => search.includes('status=FAILED'))).toBe(true);
    });
  });

  // The control is rendered from the SERVER's flag. A settled job with a stop
  // button is a button that answers 409.
  it('offers the stop control only where the server says it would work', async () => {
    seedListing([
      runtimeJob(),
      runtimeJob({ task_id: 'exec-done', status: 'SUCCEEDED', cancellable: false }),
    ]);
    renderAdminRoute(<AdminTasks refetchInterval={false} />);
    await screen.findByTestId('admin-task-exec-done');

    expect(screen.getByLabelText('Stop exec-1')).toBeInTheDocument();
    expect(screen.queryByLabelText('Stop exec-done')).toBeNull();
  });

  it('confirms before it stops a job, and then calls the cancel route', async () => {
    const seen: string[] = [];
    seedListing([runtimeJob()], seen);
    let cancelled: string | undefined;
    server.use(
      http.post(`${JOBS_URL}/:kind/:jobID`, ({ params }) => {
        cancelled = `${String(params['kind'])}/${String(params['jobID'])}`;
        return HttpResponse.json({ ok: true });
      }),
    );
    renderAdminRoute(<AdminTasks refetchInterval={false} />);
    await screen.findByTestId('admin-task-exec-1');

    await userEvent.click(screen.getByLabelText('Stop exec-1'));
    // The confirm is a real gate: nothing is sent until it is accepted.
    expect(cancelled).toBeUndefined();
    await userEvent.click(await screen.findByRole('button', { name: 'Stop job' }));

    await waitFor(() => {
      expect(cancelled).toBe('index/exec-1:cancel');
    });
  });

  // An eval run's id is unique only inside its project, so the cancel must
  // carry the project the row named.
  it('sends the project with an evaluation cancel', async () => {
    seedListing([runtimeJob({
      task_id: '31', kind: 'eval', name: '5', status: 'running', project_id: 7,
    })]);
    let search: string | undefined;
    server.use(
      http.post(`${JOBS_URL}/:kind/:jobID`, ({ request }) => {
        search = new URL(request.url).search;
        return HttpResponse.json({ ok: true });
      }),
    );
    renderAdminRoute(<AdminTasks refetchInterval={false} />);
    await screen.findByTestId('admin-task-31');

    await userEvent.click(screen.getByLabelText('Stop 31'));
    await userEvent.click(await screen.findByRole('button', { name: 'Stop job' }));

    await waitFor(() => {
      expect(search).toContain('project_id=7');
    });
  });

  it('shows the failure rather than reporting success when the stop is refused', async () => {
    seedListing([runtimeJob()]);
    server.use(
      http.post(`${JOBS_URL}/:kind/:jobID`, () =>
        HttpResponse.json({ error: 'already settled' }, { status: 409 })),
    );
    renderAdminRoute(<AdminTasks refetchInterval={false} />);
    await screen.findByTestId('admin-task-exec-1');

    await userEvent.click(screen.getByLabelText('Stop exec-1'));
    await userEvent.click(await screen.findByRole('button', { name: 'Stop job' }));

    expect(await screen.findByTestId('admin-tasks-error')).toBeInTheDocument();
  });

  // The SERVER's sentence, not a local copy of it — so the screen moves when
  // the server's answer changes.
  it('renders the server’s own reason when the listing is refused', async () => {
    server.use(http.get(JOBS_URL, () =>
      HttpResponse.json(
        { error: 'this deployment has no background-job store configured' },
        { status: 503 },
      )));
    renderAdminRoute(<AdminTasks refetchInterval={false} />);

    const notice = await screen.findByTestId('admin-tasks-unavailable');
    expect(notice).toHaveTextContent('no background-job store configured');
    // And NOT an empty table beside it: "nothing is running" and "this
    // deployment cannot see what is running" must not render the same.
    expect(screen.queryByTestId('admin-tasks-empty')).toBeNull();
  });

  // The pager is server-side: `hasNext` comes from the server's `total`, and
  // the next page is a new REQUEST with a moved offset. A pager that never
  // moved the offset would repeat page one for ever and look right.
  it('pages through the listing on the server', async () => {
    const seen: string[] = [];
    server.use(
      http.get(JOBS_URL, ({ request }) => {
        const search = new URL(request.url).search;
        seen.push(search);
        return HttpResponse.json({
          total: 120,
          rows: [runtimeJob({ task_id: search.includes('offset=50') ? 'exec-2' : 'exec-1' })],
          truncated: false,
        });
      }),
    );
    renderAdminRoute(<AdminTasks refetchInterval={false} />);
    await screen.findByTestId('admin-task-exec-1');

    expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled();
    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    await screen.findByTestId('admin-task-exec-2');
    expect(seen.some((search) => search.includes('offset=50'))).toBe(true);

    await userEvent.click(screen.getByRole('button', { name: 'Previous' }));
    await screen.findByTestId('admin-task-exec-1');
    expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled();
  });

  // A refusal with no reason in it still says the page is unavailable rather
  // than rendering an empty table.
  it('falls back to its own sentence when the refusal carries no reason', async () => {
    server.use(http.get(JOBS_URL, () => new HttpResponse(null, { status: 503 })));
    renderAdminRoute(<AdminTasks refetchInterval={false} />);
    await expect.poll(() => screen.queryByTestId('admin-tasks-unavailable')).not.toBeNull();
  });

  // The chip colour is derived from three different status vocabularies. A
  // status this page has not been taught renders uncoloured rather than green.
  it('colours the status chip for every source’s vocabulary', async () => {
    seedListing([
      runtimeJob({ task_id: 'exec-failed', status: 'FAILED', cancellable: false }),
      runtimeJob({ task_id: 'exec-stopping', status: 'CANCELLING', cancellable: false }),
      runtimeJob({ task_id: 'eval-1', kind: 'eval', status: 'errored', cancellable: false }),
      runtimeJob({ task_id: 'sched-1', kind: 'schedule', status: 'SUPERSEDED', cancellable: false }),
    ]);
    renderAdminRoute(<AdminTasks refetchInterval={false} />);
    await screen.findByTestId('admin-task-sched-1');

    for (const [taskId, status] of [
      ['exec-failed', 'FAILED'],
      ['exec-stopping', 'CANCELLING'],
      ['eval-1', 'errored'],
      ['sched-1', 'SUPERSEDED'],
    ] as const) {
      expect(within(screen.getByTestId(`admin-task-${taskId}`)).getByText(status)).toBeInTheDocument();
    }
  });

  // An absent timestamp is a dash, never an empty cell: a blank Finished column
  // reads as a rendering fault rather than as "still running".
  it('renders an absent timestamp as a dash', async () => {
    seedListing([runtimeJob({ started_at: null, finished_at: null, user: '' })]);
    renderAdminRoute(<AdminTasks refetchInterval={false} />);
    const row = await screen.findByTestId('admin-task-exec-1');
    expect(within(row).getAllByText('\u2014').length).toBeGreaterThanOrEqual(3);
  });

  it('says so when the server’s scan window was full', async () => {
    seedListing([runtimeJob()], undefined, true);
    renderAdminRoute(<AdminTasks refetchInterval={false} />);
    expect(await screen.findByTestId('admin-tasks-truncated')).toBeInTheDocument();
  });
});
