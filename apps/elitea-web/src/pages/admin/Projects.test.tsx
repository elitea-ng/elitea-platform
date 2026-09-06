/**
 * Rendering + write-path guard for `pages/admin/Projects.tsx` (unit A14).
 *
 * The properties asserted here are the ones the reference page's own defects,
 * and this unit's server-side findings, show are worth asserting:
 *
 *  1. Rows render from the measured `{rows,total,counts}` body, with the three
 *     columns the pre-A14 server did not send (`owner_name`, `admin_names`,
 *     `status`) each coming from the RESPONSE rather than from a constant.
 *  2. Every enabled control REACHES THE SERVER with the body pylon defined.
 *     A control that renders but sends nothing is the class #130/#180 shipped;
 *     asserting only that a button exists would not catch it.
 *  3. Every control with NO server behind it is disabled with a stated reason —
 *     and the reason is the real one, not a placeholder. Only the Excel export
 *     is in that state now; create and delete left it when the provisioning
 *     pipeline landed (#333).
 *  4. Every write is absent entirely when its permission is absent, and absent
 *     means "not rendered", not "rendered and ignored". Create and delete are
 *     gated on their OWN permissions, not on the suspend one.
 *  5. Delete cannot fire on a mis-click, and says by name what it is about to
 *     destroy. It is the only irreversible control on the page.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminProjects } from './Projects';
import { renderAdminRoute } from './__tests__/testRouter';

/**
 * Measured against the Go handler in
 * `services/elitea-main/internal/api/v2/admin/projects.go`: rows carry
 * `owner_name`, `admin_names`, `status`, `is_personal`; `counts` is unfiltered
 * and labels the tabs; `total` describes the FILTERED set.
 */
const PROJECTS_BODY = {
  rows: [
    {
      id: 41,
      name: 'atlas',
      owner_id: 7,
      owner_name: 'Ada Owner',
      admin_names: ['Bo Admin', 'Cy Admin'],
      status: 'active',
      suspended: false,
      create_success: true,
      is_personal: false,
    },
    {
      id: 42,
      name: 'borealis',
      owner_id: 8,
      owner_name: 'Bea Owner',
      admin_names: [],
      status: 'suspended',
      suspended: true,
      create_success: true,
      is_personal: false,
    },
    {
      id: 43,
      name: 'cirrus',
      owner_id: 9,
      owner_name: '',
      admin_names: [],
      status: 'failed',
      suspended: false,
      create_success: false,
      is_personal: false,
    },
  ],
  total: 3,
  counts: { team: 3, personal: 12 },
};

const PROJECT_ROLES = [
  { id: '1', name: 'admin' },
  { id: '2', name: 'editor' },
  { id: '3', name: 'viewer' },
];

const PROJECT_MEMBERS = {
  rows: [{ id: '77', email: 'already@example.com', name: 'Al Ready', roles: ['viewer'] }],
  total: 1,
};

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

/** The full admin-panel surface this page touches: two reads plus three writes. */
function useAdminProjectHandlers(): void {
  server.use(
    http.get('*/admin/projects/administration', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json(PROJECTS_BODY);
    }),
    http.put('*/admin/project_suspend/administration/*', async ({ request }) => {
      recorded.push({ method: 'PUT-suspend', url: request.url, body: await request.json() });
      return HttpResponse.json({ id: 41, suspended: true });
    }),
    http.get('*/admin/roles/administration/*', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json(PROJECT_ROLES);
    }),
    http.get('*/admin/users/administration/*', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json(PROJECT_MEMBERS);
    }),
    http.post('*/admin/users/administration/*', async ({ request }) => {
      recorded.push({ method: 'POST-member', url: request.url, body: await request.json() });
      return HttpResponse.json([{ status: 'ok', msg: 'added', email: 'new@example.com' }]);
    }),
    http.put('*/admin/users/administration/*', async ({ request }) => {
      recorded.push({ method: 'PUT-member', url: request.url, body: await request.json() });
      return HttpResponse.json({ msg: 'roles updated' });
    }),
    // Provisioning. NOT on the `/admin/...` family: create and delete belong to
    // the projects handler, which drives the step pipeline.
    http.post('*/projects/project/administration', async ({ request }) => {
      recorded.push({ method: 'POST-create', url: request.url, body: await request.json() });
      return HttpResponse.json(
        { id: 99, steps: [{ step: 'project_model', initialized: true, ok: true, msg: '' }], rollback_steps: [] },
        { status: 201 },
      );
    }),
    http.delete('*/projects/project/administration/*', ({ request }) => {
      recorded.push({ method: 'DELETE-project', url: request.url, body: null });
      return HttpResponse.json({ steps: [] });
    }),
  );
}

/** Every permission the page's controls read, for the default-granted case. */
const ALL_PROJECT_PERMISSIONS = [
  'projects.projects.projects.view',
  'projects.projects.projects.edit',
  'projects.projects.project.create',
  'projects.projects.project.delete',
];

/** Tick one project's selection checkbox, by the name in its row. */
async function selectRow(user: ReturnType<typeof userEvent.setup>, name: string): Promise<void> {
  const row = dataRows().find((candidate) => within(candidate).queryByText(name) !== null);
  if (!row) throw new Error(`no row for ${name}`);
  await user.click(within(row).getByRole('checkbox'));
}

/** The permission list the Go adminui handler injects for a valid session. */
function grantAdminUiPermissions(permissions: string[]): void {
  window.admin_ui_config = { permissions, vite_server_url: '/api/v2' };
}

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method !== 'GET');
}

function dataRows(): HTMLElement[] {
  return within(screen.getByRole('grid'))
    .getAllByRole('row')
    .filter((row) => row.getAttribute('data-id'));
}

beforeEach(() => {
  recorded = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  grantAdminUiPermissions(ALL_PROJECT_PERMISSIONS);
  useAdminProjectHandlers();
});

afterEach(() => {
  resetGeneratedClient();
  delete window.admin_ui_config;
  // The export tests stub URL.createObjectURL and anchor clicks; leaking those
  // into a later file would make its downloads silently no-op.
  vi.restoreAllMocks();
});

describe('Admin › Projects', () => {
  it('renders one row per project, with owner, admins and status from the response', async () => {
    renderAdminRoute(<AdminProjects />);

    expect(await screen.findByText('atlas')).toBeInTheDocument();
    expect(dataRows()).toHaveLength(3);

    const grid = screen.getByRole('grid');
    // `owner_name` and `admin_names` are the two fields the pre-A14 server did
    // not send at all — it emitted a bare `owner_id`. If either were dropped,
    // the columns would render blank and the row count alone would not notice.
    expect(within(grid).getByText('Ada Owner')).toBeInTheDocument();
    expect(within(grid).getByText('Bo Admin, Cy Admin')).toBeInTheDocument();

    // All three statuses come from the server's `status` field. The admin Users
    // reference page read a `status` that never existed and rendered one
    // constant; asserting all three is what separates the two readings.
    expect(within(grid).getByText('Active')).toBeInTheDocument();
    expect(within(grid).getByText('Suspended')).toBeInTheDocument();
    expect(within(grid).getByText('Failed')).toBeInTheDocument();

    expect(screen.queryByText('No projects')).not.toBeInTheDocument();
  });

  it('labels the tabs from `counts`, which is not the page total', async () => {
    renderAdminRoute(<AdminProjects />);
    await screen.findByText('atlas');

    // 12 personal projects with 3 rows on the page: a tab label taken from
    // `total`, or from `rows.length`, would read (3) here.
    expect(screen.getByRole('tab', { name: 'Team Projects (3)' })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'Personal Projects (12)' })).toBeInTheDocument();
  });

  it('asks the server for the personal tab rather than filtering the page client-side', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminProjects />);
    await screen.findByText('atlas');

    await user.click(screen.getByRole('tab', { name: 'Personal Projects (12)' }));

    await waitFor(() =>
      expect(
        recorded.some((entry) => entry.url.includes('project_type=personal')),
      ).toBe(true),
    );
  });

  it('suspends a project by PUTting {suspended:true} to the real endpoint', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminProjects />);
    await screen.findByText('atlas');

    await user.click(screen.getAllByRole('button', { name: 'Suspend project' })[0]!);

    await waitFor(() => expect(writes()).toHaveLength(1));
    const request = writes()[0]!;
    expect(request.method).toBe('PUT-suspend');
    // The id is in the PATH, mirroring pylon's `project_suspend/<mode>/<project_id>`.
    expect(request.url).toContain('/admin/project_suspend/administration/41');
    expect(request.body).toEqual({ suspended: true });
  });

  it('unsuspends the already-suspended project (the toggle reads the row, not a constant)', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminProjects />);
    await screen.findByText('borealis');

    await user.click(screen.getByRole('button', { name: 'Unsuspend project' }));

    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(writes()[0]!.body).toEqual({ suspended: false });
    expect(writes()[0]!.url).toContain('/admin/project_suspend/administration/42');
  });

  it('reports a refused suspend instead of leaving the row unchanged and silent', async () => {
    server.use(
      http.put('*/admin/project_suspend/administration/*', () =>
        HttpResponse.json({ error: 'insufficient permissions' }, { status: 403 }),
      ),
    );
    const user = userEvent.setup();
    renderAdminRoute(<AdminProjects />);
    await screen.findByText('atlas');

    await user.click(screen.getAllByRole('button', { name: 'Suspend project' })[0]!);

    // The reference swallows every failure, so a 403 there is indistinguishable
    // from "nothing happened" — and suspension's whole feedback is a row's
    // opacity changing.
    expect(await screen.findByRole('alert')).toBeInTheDocument();
  });

  describe('create', () => {
    it('POSTs the pylon create body, and sends neither an owner nor a role', async () => {
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      await user.click(screen.getByRole('button', { name: 'Create project' }));
      await user.type(await screen.findByRole('textbox', { name: 'Project name' }), 'cygnus');
      await user.click(screen.getByRole('button', { name: 'Create' }));

      await waitFor(() => expect(writes()).toHaveLength(1));
      expect(writes()[0]!.method).toBe('POST-create');
      expect(writes()[0]!.url).toContain('/projects/project/administration');
      // Owner and role are server-side facts. A body carrying either would be a
      // privilege decision made in a form — the server ignores both, and this
      // asserts the client does not try.
      expect(writes()[0]!.body).toEqual({ name: 'cygnus', project_admin_email: [] });
    });

    it('refuses an empty name without a round trip', async () => {
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      await user.click(screen.getByRole('button', { name: 'Create project' }));
      await user.click(await screen.findByRole('button', { name: 'Create' }));

      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(writes()).toHaveLength(0);
    });

    it('names the STEP that failed, not just that something did', async () => {
      server.use(
        http.post('*/projects/project/administration', () =>
          HttpResponse.json(
            {
              steps: [
                { step: 'project_model', initialized: true, ok: true, msg: '' },
                {
                  step: 'project_schema',
                  initialized: true,
                  ok: false,
                  msg: 'step project_schema did not complete',
                },
              ],
              rollback_steps: [],
            },
            { status: 500 },
          ),
        ),
      );
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      await user.click(screen.getByRole('button', { name: 'Create project' }));
      await user.type(await screen.findByRole('textbox', { name: 'Project name' }), 'cygnus');
      await user.click(screen.getByRole('button', { name: 'Create' }));

      // The step list lives ONLY on the rejected response's body. A client that
      // kept the status and dropped the body would reduce "the tenant schema
      // step failed" to a bare 500.
      expect(await screen.findByText(/project_schema/)).toBeInTheDocument();
      // The steps that SUCCEEDED are not reported: nine green lines bury the
      // one red one.
      expect(screen.queryByText(/project_model/)).not.toBeInTheDocument();
    });

    it('separates a failed ROLLBACK from a failed forward step', async () => {
      server.use(
        http.post('*/projects/project/administration', () =>
          HttpResponse.json(
            {
              // The SAME step name in both lists, which is what the server
              // really emits: compensate() undoes every attempted step,
              // including the one that failed.
              steps: [
                {
                  step: 'project_schema',
                  initialized: true,
                  ok: false,
                  msg: 'step project_schema did not complete',
                },
              ],
              rollback_steps: [
                {
                  step: 'project_schema',
                  initialized: true,
                  ok: false,
                  msg: 'step project_schema was not started, because the project row is still there',
                },
              ],
            },
            { status: 500 },
          ),
        ),
      );
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      await user.click(screen.getByRole('button', { name: 'Create project' }));
      await user.type(await screen.findByRole('textbox', { name: 'Project name' }), 'cygnus');
      await user.click(screen.getByRole('button', { name: 'Create' }));

      // Two headings, because the two call for opposite operator actions: a
      // forward failure is already cleaned up, a rollback failure is
      // infrastructure left behind. Flattening them into one list loses that.
      expect(await screen.findByText(/Provisioning stopped at/)).toBeInTheDocument();
      expect(screen.getByText(/Cleanup did not finish/)).toBeInTheDocument();
      expect(screen.getByText(/did not complete/)).toBeInTheDocument();
      expect(screen.getByText(/was not started/)).toBeInTheDocument();
    });

    /**
     * The half of criterion 1 (#377) that lives in the client: "the new project
     * APPEARS IN THE LIST".
     *
     * The POST landing is not enough. The row reaches the screen only because
     * the mutation invalidates `adminProjectsKeys.all`, and a mutation that
     * invalidated a key the listing does not use would leave an operator
     * looking at a list without the project they had just made — the API is
     * right and the screen is empty, which is the shape #132 shipped twice.
     *
     * The listing answers DIFFERENTLY the second time, so the assertion can only
     * pass on a real refetch. J31g asserts the same property against the real
     * server, through a full reload.
     */
    it('puts the created project on screen, because the create refreshes the listing', async () => {
      let listings = 0;
      server.use(
        http.get('*/admin/projects/administration', () => {
          listings += 1;
          if (listings === 1) return HttpResponse.json(PROJECTS_BODY);
          return HttpResponse.json({
            ...PROJECTS_BODY,
            rows: [
              ...PROJECTS_BODY.rows,
              {
                id: 99,
                name: 'cygnus',
                owner_id: 7,
                owner_name: 'Ada Owner',
                admin_names: ['Bo Admin'],
                status: 'active',
                suspended: false,
                create_success: true,
                is_personal: false,
              },
            ],
            total: 4,
            counts: { team: 4, personal: 12 },
          });
        }),
      );
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');
      expect(screen.queryByText('cygnus')).not.toBeInTheDocument();

      await user.click(screen.getByRole('button', { name: 'Create project' }));
      await user.type(await screen.findByRole('textbox', { name: 'Project name' }), 'cygnus');
      await user.click(screen.getByRole('button', { name: 'Create' }));

      expect(await screen.findByText('cygnus')).toBeInTheDocument();
      expect(listings).toBeGreaterThan(1);
    });
  });

  describe('delete', () => {
    it('is disabled until something is selected', async () => {
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      expect(screen.getByRole('button', { name: 'Delete projects' })).toBeDisabled();
    });

    it('sends nothing until the confirmation word is typed', async () => {
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      await selectRow(user, 'atlas');
      await user.click(screen.getByRole('button', { name: 'Delete projects' }));

      // The dialog must list what it is about to destroy, by name.
      const list = await screen.findByTestId('admin-projects-delete-list');
      expect(within(list).getByText('atlas (ID: 41)')).toBeInTheDocument();

      // Armed only by the typed word. This is the one control on the page whose
      // effect cannot be undone, so a mis-click must not be enough.
      expect(screen.getByRole('button', { name: 'Delete permanently' })).toBeDisabled();
      expect(writes()).toHaveLength(0);
    });

    it('DELETEs each selected project once the word is typed', async () => {
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      await selectRow(user, 'atlas');
      await selectRow(user, 'borealis');
      await user.click(screen.getByRole('button', { name: 'Delete projects' }));

      await user.type(
        await screen.findByRole('textbox', { name: 'Type DELETE to confirm' }),
        'DELETE',
      );
      await user.click(screen.getByRole('button', { name: 'Delete permanently' }));

      // One request per project: there is no batch route.
      await waitFor(() => expect(writes()).toHaveLength(2));
      expect(writes().map((entry) => entry.method)).toEqual(['DELETE-project', 'DELETE-project']);
      expect(writes()[0]!.url).toContain('/projects/project/administration/41');
      expect(writes()[1]!.url).toContain('/projects/project/administration/42');
    });

    it('reports the projects it could not delete, by name', async () => {
      server.use(
        http.delete('*/projects/project/administration/42', () =>
          HttpResponse.json({ steps: [] }, { status: 500 }),
        ),
      );
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      await selectRow(user, 'atlas');
      await selectRow(user, 'borealis');
      await user.click(screen.getByRole('button', { name: 'Delete projects' }));
      await user.type(
        await screen.findByRole('textbox', { name: 'Type DELETE to confirm' }),
        'DELETE',
      );
      await user.click(screen.getByRole('button', { name: 'Delete permanently' }));

      // A refusal on one must not spare the rest, and "three of five" is the
      // only honest report when it happens.
      expect(await screen.findByText(/borealis:/)).toBeInTheDocument();
      expect(screen.queryByText(/atlas:/)).not.toBeInTheDocument();
    });

    /**
     * The half of criterion 3 (#377) that lives in the client: "the project
     * LEAVES THE LIST".
     *
     * The loop invalidates ONCE, after every DELETE has been sent — invalidating
     * per mutation would make each iteration wait for a full listing refetch
     * before the next request went out. So the refresh is written in a different
     * place from the write it belongs to, and nothing else asserts that the two
     * still meet.
     *
     * The listing answers DIFFERENTLY the second time, so this can only pass on
     * a real refetch. J31g asserts the same property against the real server.
     */
    it('takes the deleted project off screen, because the delete refreshes the listing', async () => {
      let listings = 0;
      server.use(
        http.get('*/admin/projects/administration', () => {
          listings += 1;
          if (listings === 1) return HttpResponse.json(PROJECTS_BODY);
          return HttpResponse.json({
            ...PROJECTS_BODY,
            rows: PROJECTS_BODY.rows.filter((row) => row.name !== 'atlas'),
            total: 2,
            counts: { team: 2, personal: 12 },
          });
        }),
      );
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      await selectRow(user, 'atlas');
      await user.click(screen.getByRole('button', { name: 'Delete projects' }));
      await user.type(
        await screen.findByRole('textbox', { name: 'Type DELETE to confirm' }),
        'DELETE',
      );
      await user.click(screen.getByRole('button', { name: 'Delete permanently' }));

      await waitFor(() => expect(screen.queryByText('atlas')).not.toBeInTheDocument());
      // The refresh removed ONE row, not the table: a listing that failed to
      // render at all would also have no `atlas` in it.
      expect(screen.getByText('borealis')).toBeInTheDocument();
      expect(listings).toBeGreaterThan(1);
    });
  });

  /**
   * The export used to be a disabled button; asserting only that it is now
   * ENABLED would pass against a control that downloads an empty file. These
   * two assert the file's actual bytes and that a refusal is surfaced — the
   * "renders but sends nothing" class this file exists to fence.
   */
  it('exports every row the current filter selects, as CSV', async () => {
    const user = userEvent.setup();
    let exported: Blob | undefined;
    vi.spyOn(URL, 'createObjectURL').mockImplementation((blob: Blob | MediaSource) => {
      exported = blob as Blob;
      return 'blob:mock-url';
    });
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);

    renderAdminRoute(<AdminProjects />);
    await screen.findByText('atlas');

    await user.click(screen.getByRole('button', { name: 'Export to CSV' }));
    await waitFor(() => expect(clickSpy).toHaveBeenCalledTimes(1));

    // The UTF-8 BOM is what makes Excel decode the file as UTF-8, so it is
    // asserted on the BYTES: `Blob.text()` strips a leading BOM per spec, and
    // an assertion on the decoded string would pass without it.
    const bytes = new Uint8Array(await exported!.arrayBuffer());
    expect([bytes[0], bytes[1], bytes[2]]).toEqual([0xef, 0xbb, 0xbf]);

    const lines = (await exported!.text()).split('\r\n');
    expect(lines[0]).toBe('Name,ID,Owner,Admins,Status');
    // The joined admin list carries a comma, so the cell has to be quoted.
    expect(lines[1]).toBe('atlas,41,Ada Owner,"Bo Admin, Cy Admin",Active');
    // Status is the server's derived field — the same reading the chip makes.
    expect(lines[2]).toBe('borealis,42,Bea Owner,,Suspended');
    expect(lines[3]).toBe('cirrus,43,,,Failed');

    // The export walks the LIST endpoint, filtered by the active tab.
    const exportRead = recorded.filter((entry) => entry.url.includes('/admin/projects/')).at(-1)!;
    expect(exportRead.url).toContain('project_type=team');
    // 100, not a bigger number: the admin handler ignores a `limit` above 100
    // and silently serves 20, which the walk would read as the last page.
    expect(exportRead.url).toContain('limit=100');
  });

  it('reports a refused export instead of downloading an empty file', async () => {
    const user = userEvent.setup();
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    renderAdminRoute(<AdminProjects />);
    await screen.findByText('atlas');

    server.use(
      http.get('*/admin/projects/administration', () =>
        HttpResponse.json({ error: 'insufficient permissions' }, { status: 403 }),
      ),
    );

    await user.click(screen.getByRole('button', { name: 'Export to CSV' }));

    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(clickSpy).not.toHaveBeenCalled();
  });

  describe('without the project-write permission', () => {
    beforeEach(() => {
      grantAdminUiPermissions(['projects.projects.projects.view']);
    });

    it('renders no suspend or member control at all', async () => {
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      expect(screen.queryByRole('button', { name: 'Suspend project' })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Unsuspend project' })).not.toBeInTheDocument();
      expect(
        screen.queryByRole('button', { name: 'Manage project member' }),
      ).not.toBeInTheDocument();
    });

    it('still renders the read-only activity control', async () => {
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      expect(screen.getAllByRole('button', { name: 'Project activity' })).not.toHaveLength(0);
    });

    it('renders neither provisioning control, nor the selection they arm', async () => {
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      // `projects.projects.projects.edit` is granted in this block's parent and
      // NOT here — but even with it, create and delete are gated on their own
      // permissions. Suspending is reversible and dropping a schema is not, so
      // the two must not travel together.
      expect(screen.queryByRole('button', { name: 'Create project' })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Delete projects' })).not.toBeInTheDocument();
      expect(screen.queryAllByRole('checkbox')).toHaveLength(0);
    });

    it('shows create without delete when only create is granted', async () => {
      grantAdminUiPermissions([
        'projects.projects.projects.view',
        'projects.projects.project.create',
      ]);
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      expect(screen.getByRole('button', { name: 'Create project' })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Delete projects' })).not.toBeInTheDocument();
    });
  });

  describe('the member dialog', () => {
    it('offers the roles the SERVER defines, not a hardcoded list', async () => {
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');

      await user.click(screen.getAllByRole('button', { name: 'Manage project member' })[0]!);

      await waitFor(() =>
        expect(recorded.some((entry) => entry.url.includes('/admin/roles/administration/41'))).toBe(
          true,
        ),
      );
      await user.click(await screen.findByRole('combobox', { name: 'Role' }));
      expect(await screen.findByRole('option', { name: 'editor' })).toBeInTheDocument();
    });

    it('POSTs the pylon invite body for an address that is not yet a member', async () => {
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');
      await user.click(screen.getAllByRole('button', { name: 'Manage project member' })[0]!);

      await user.type(await screen.findByRole('textbox', { name: 'User email' }), 'new@example.com');
      await waitFor(() =>
        expect(screen.getByRole('button', { name: 'Add user' })).toBeEnabled(),
      );
      await user.click(screen.getByRole('button', { name: 'Add user' }));

      await waitFor(() => expect(writes()).toHaveLength(1));
      expect(writes()[0]!.method).toBe('POST-member');
      expect(writes()[0]!.url).toContain('/admin/users/administration/41');
      expect(writes()[0]!.body).toEqual({ emails: ['new@example.com'], roles: ['admin'] });
    });

    it('PUTs a role replacement for an address that is already a member', async () => {
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');
      await user.click(screen.getAllByRole('button', { name: 'Manage project member' })[0]!);

      // Deliberately a different case from the seeded member: the server
      // lower-cases before comparing, so this IS the same person and an "Add"
      // here would be refused as a duplicate.
      await user.type(
        await screen.findByRole('textbox', { name: 'User email' }),
        'Already@Example.com',
      );
      await waitFor(() =>
        expect(screen.getByRole('button', { name: 'Update role' })).toBeEnabled(),
      );
      await user.click(screen.getByRole('button', { name: 'Update role' }));

      await waitFor(() => expect(writes()).toHaveLength(1));
      expect(writes()[0]!.method).toBe('PUT-member');
      expect(writes()[0]!.body).toEqual({ id: '77', roles: ['admin'] });
    });

    it('refuses to submit an address that is not one, without a server round trip', async () => {
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');
      await user.click(screen.getAllByRole('button', { name: 'Manage project member' })[0]!);

      await user.type(await screen.findByRole('textbox', { name: 'User email' }), 'not-an-address');

      expect(screen.getByRole('button', { name: 'Add user' })).toBeDisabled();
      expect(writes()).toHaveLength(0);
    });

    it('reports a rejected write instead of showing a success banner', async () => {
      server.use(
        http.post('*/admin/users/administration/*', () =>
          HttpResponse.json({ error: 'unknown role(s) for this project: admin' }, { status: 400 }),
        ),
      );
      const user = userEvent.setup();
      renderAdminRoute(<AdminProjects />);
      await screen.findByText('atlas');
      await user.click(screen.getAllByRole('button', { name: 'Manage project member' })[0]!);

      await user.type(await screen.findByRole('textbox', { name: 'User email' }), 'new@example.com');
      await waitFor(() => expect(screen.getByRole('button', { name: 'Add user' })).toBeEnabled());
      await user.click(screen.getByRole('button', { name: 'Add user' }));

      // The reference inspects `result[0].status` on a RESOLVED promise, so a
      // 400 there never reaches its error branch at all.
      const alert = await screen.findByRole('alert');
      expect(alert).toBeInTheDocument();
      expect(screen.queryByText('User added to the project.')).not.toBeInTheDocument();
    });
  });

  it('renders the empty state rather than a blank grid when there are no projects', async () => {
    server.use(
      http.get('*/admin/projects/administration', () =>
        HttpResponse.json({ rows: [], total: 0, counts: { team: 0, personal: 0 } }),
      ),
    );
    renderAdminRoute(<AdminProjects />);

    expect(await screen.findByText('No projects')).toBeInTheDocument();
  });

  it('reports a failed listing instead of rendering it as an empty deployment', async () => {
    server.use(
      http.get('*/admin/projects/administration', () =>
        HttpResponse.json({ error: 'failed to list projects' }, { status: 500 }),
      ),
    );
    renderAdminRoute(<AdminProjects />);

    expect(await screen.findByText('Failed to load projects.')).toBeInTheDocument();
  });
});
