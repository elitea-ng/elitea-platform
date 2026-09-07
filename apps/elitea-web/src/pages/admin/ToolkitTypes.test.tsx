/**
 * `Admin › Toolkits`, driven against the real generated-client mutator and MSW.
 *
 * Nothing is `vi.mock`'d: the point of most of these cases is the wire — the
 * envelope the client must unwrap, the body each write must send, and the
 * server sentence the page must render instead of inventing one.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminToolkitTypes } from './ToolkitTypes';
import { renderAdminRoute } from './__tests__/testRouter';

const LIST_PATTERN = '*/admin/toolkit_types/administration';
const TYPE_PATTERN = '*/admin/toolkit_types/administration/:type';
const GRANT_PATTERN = '*/admin/toolkit_types/administration/:type/projects/:projectID';
const BULK_PATTERN = '*/admin/toolkit_types/administration/bulk';

interface RecordedWrite {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let writes: RecordedWrite[] = [];

const GITHUB_ROW = {
  type: 'github',
  label: 'GitHub',
  category: 'code_repositories',
  availability: 'default',
  reason: '',
  decided_by: '',
  decided_at: '',
  available: true,
  source: 'default',
  registered: true,
  capability: {
    verdict: 'supported',
    python: true,
    rust: true,
    reason: 'the admitted Python worker image build-verifies this type import',
  },
  project_grants: [],
};

const SQL_ROW = {
  type: 'sql',
  label: 'SQL',
  category: 'development',
  availability: 'restricted',
  reason: 'offered per contract',
  decided_by: 'operator@example.com',
  decided_at: '2026-09-06T00:00:00Z',
  available: false,
  source: 'deployment',
  registered: true,
  capability: {
    verdict: 'unverified',
    python: false,
    rust: true,
    reason: 'the Rust worker carries a native family for this type',
  },
  project_grants: [
    {
      project_id: 7,
      availability: 'enabled',
      reason: 'contract 4471',
      granted_by: 'operator@example.com',
      granted_at: '2026-09-06T00:00:00Z',
    },
  ],
};

function serveListing(body: Record<string, unknown>, status = 200) {
  server.use(http.get(LIST_PATTERN, () => HttpResponse.json(body, { status })));
}

function serveDefaultListing() {
  serveListing({
    types: [GITHUB_ROW, SQL_ROW],
    categories: ['code_repositories', 'development'],
    total: 2,
    registry_available: true,
  });
}

function recordWrites(status = 200) {
  const record = async ({ request }: { request: Request }) => {
    let body: unknown = null;
    try {
      body = await request.clone().json();
    } catch {
      body = null;
    }
    writes.push({ method: request.method, url: request.url, body });
    return HttpResponse.json({ ok: true }, { status });
  };
  server.use(
    // The bulk route is a STATIC segment and must be registered before the
    // `:type` pattern, or `bulk` matches as a type name and the test measures
    // the wrong route.
    http.post(BULK_PATTERN, record),
    http.put(GRANT_PATTERN, record),
    http.delete(GRANT_PATTERN, record),
    http.put(TYPE_PATTERN, record),
  );
}

beforeEach(() => {
  writes = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AdminToolkitTypes', () => {
  it('renders the served types with their decision, source and worker verdict', async () => {
    serveDefaultListing();
    renderAdminRoute(<AdminToolkitTypes />);

    expect(await screen.findByText('GitHub')).toBeInTheDocument();
    expect(await screen.findByText('SQL')).toBeInTheDocument();
    // A type nobody decided about reads as Default and must NOT read as a
    // refusal — the absence rule, at the screen.
    expect(await screen.findByText('Default')).toBeInTheDocument();
    expect(await screen.findByText('Restricted')).toBeInTheDocument();
    expect(await screen.findByText('Deployment · operator@example.com')).toBeInTheDocument();
    expect(await screen.findByText('Carried')).toBeInTheDocument();
    expect(await screen.findByText('Unverified')).toBeInTheDocument();
    expect(screen.queryByTestId('admin-toolkit-types-error')).not.toBeInTheDocument();
    expect(screen.queryByTestId('admin-toolkit-types-unavailable')).not.toBeInTheDocument();
  });

  it('unwraps the transport envelope rather than reading the envelope (#132)', async () => {
    // The endpoint's real answer under the generated mutator is an envelope.
    // Reading `types` off it renders an empty grid against a perfectly good 200
    // — the silent empty state #132 records.
    serveDefaultListing();
    const { queryClient } = renderAdminRoute(<AdminToolkitTypes />);

    expect(await screen.findByText('GitHub')).toBeInTheDocument();
    await waitFor(() => {
      expect(queryClient.getQueryData(['admin', 'toolkit-types', 'list'])).toMatchObject({
        categories: ['code_repositories', 'development'],
        registryAvailable: true,
      });
    });
  });

  it('renders the server sentence for a 503 rather than an empty grid', async () => {
    serveListing(
      { error: 'the toolkit type policy store is not configured on this deployment' },
      503,
    );
    renderAdminRoute(<AdminToolkitTypes />);

    const notice = await screen.findByTestId('admin-toolkit-types-unavailable');
    expect(notice).toHaveTextContent('not configured on this deployment');
    expect(screen.queryByTestId('admin-toolkit-types-error')).not.toBeInTheDocument();
  });

  it('tells a load failure apart from an unavailable surface', async () => {
    serveListing({ error: 'boom' }, 500);
    renderAdminRoute(<AdminToolkitTypes />);

    expect(await screen.findByTestId('admin-toolkit-types-error')).toBeInTheDocument();
    expect(screen.queryByTestId('admin-toolkit-types-unavailable')).not.toBeInTheDocument();
  });

  it('says the registry could not be enumerated instead of showing an empty platform', async () => {
    serveListing({ types: [], categories: [], total: 0, registry_available: false });
    renderAdminRoute(<AdminToolkitTypes />);

    expect(
      await screen.findByTestId('admin-toolkit-types-registry-unavailable'),
    ).toBeInTheDocument();
  });

  it('filters by search over both the label and the type key', async () => {
    serveDefaultListing();
    const user = userEvent.setup();
    renderAdminRoute(<AdminToolkitTypes />);
    expect(await screen.findByText('GitHub')).toBeInTheDocument();

    // The KEY, not the label: an operator reading a log or a guardrails list
    // has the key.
    await user.type(await screen.findByLabelText('Search'), 'sql');
    await waitFor(() => {
      expect(screen.queryByText('GitHub')).not.toBeInTheDocument();
    });
    expect(screen.getByText('SQL')).toBeInTheDocument();
  });
});

describe('AdminToolkitTypes decisions', () => {
  it('refuses to submit without a reason, and sends the decision when there is one', async () => {
    serveDefaultListing();
    recordWrites();
    const user = userEvent.setup();
    renderAdminRoute(<AdminToolkitTypes />);

    await user.click((await screen.findAllByRole('button', { name: 'Decide' }))[0]!);
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('radio', { name: /Disabled/ }));

    const save = within(dialog).getByRole('button', { name: 'Save decision' });
    expect(save).toBeDisabled();

    // Whitespace is not a reason. The button must stay disabled and nothing
    // may leave the page.
    await user.type(within(dialog).getByLabelText(/Reason/), '   ');
    expect(save).toBeDisabled();
    expect(writes).toHaveLength(0);

    await user.clear(within(dialog).getByLabelText(/Reason/));
    await user.type(within(dialog).getByLabelText(/Reason/), 'the worker image does not carry it');
    await user.click(save);

    await waitFor(() => {
      expect(writes).toHaveLength(1);
    });
    expect(writes[0]!.method).toBe('PUT');
    expect(writes[0]!.url).toContain('/admin/toolkit_types/administration/github');
    expect(writes[0]!.body).toEqual({
      availability: 'disabled',
      reason: 'the worker image does not carry it',
    });
  });

  it('reverts to the default without demanding a reason', async () => {
    // `default` DELETES the row server-side, so there is nothing left to carry
    // a reason and the dialog must not insist on one.
    serveDefaultListing();
    recordWrites();
    const user = userEvent.setup();
    renderAdminRoute(<AdminToolkitTypes />);

    await user.click((await screen.findAllByRole('button', { name: 'Decide' }))[0]!);
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('radio', { name: /Default/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Save decision' }));

    await waitFor(() => {
      expect(writes).toHaveLength(1);
    });
    expect(writes[0]!.body).toEqual({ availability: 'default', reason: '' });
  });

  it('keeps the dialog open and the typing intact when the server refuses', async () => {
    serveDefaultListing();
    server.use(
      http.put(TYPE_PATTERN, () =>
        HttpResponse.json({ error: 'a reason is required' }, { status: 400 }),
      ),
    );
    const user = userEvent.setup();
    renderAdminRoute(<AdminToolkitTypes />);

    await user.click((await screen.findAllByRole('button', { name: 'Decide' }))[0]!);
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('radio', { name: /Disabled/ }));
    await user.type(within(dialog).getByLabelText(/Reason/), 'reviewed');
    await user.click(within(dialog).getByRole('button', { name: 'Save decision' }));

    expect(await screen.findByTestId('admin-toolkit-types-write-error')).toHaveTextContent(
      'a reason is required',
    );
    expect(within(dialog).getByLabelText(/Reason/)).toHaveValue('reviewed');
  });
});

describe('AdminToolkitTypes project exceptions', () => {
  it('offers the projects control only for a type that carries a decision', async () => {
    // The server answers 409 for a grant with no decision, so a control on a
    // `default` row would be a button whose only outcome is a refusal.
    serveDefaultListing();
    renderAdminRoute(<AdminToolkitTypes />);

    expect(await screen.findByText('GitHub')).toBeInTheDocument();
    expect(await screen.findAllByRole('button', { name: 'Decide' })).toHaveLength(2);
    expect(await screen.findAllByRole('button', { name: 'Projects' })).toHaveLength(1);
  });

  it('lists the recorded exceptions, adds one, and revokes one', async () => {
    serveDefaultListing();
    recordWrites();
    const user = userEvent.setup();
    renderAdminRoute(<AdminToolkitTypes />);

    await user.click(await screen.findByRole('button', { name: 'Projects' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByTestId('admin-toolkit-types-grants')).toHaveTextContent(
      'contract 4471',
    );

    await user.type(within(dialog).getByLabelText(/Project ID/), '9');
    await user.type(within(dialog).getByLabelText(/Reason/), 'the second client bought it');
    await user.click(within(dialog).getByRole('button', { name: 'Add exception' }));

    await waitFor(() => {
      expect(writes).toHaveLength(1);
    });
    expect(writes[0]!.method).toBe('PUT');
    expect(writes[0]!.url).toContain('/admin/toolkit_types/administration/sql/projects/9');
    expect(writes[0]!.body).toEqual({
      availability: 'enabled',
      reason: 'the second client bought it',
    });

    await user.click(within(dialog).getByRole('button', { name: 'Revoke' }));
    await waitFor(() => {
      expect(writes).toHaveLength(2);
    });
    expect(writes[1]!.method).toBe('DELETE');
    expect(writes[1]!.url).toContain('/admin/toolkit_types/administration/sql/projects/7');
  });

  it('refuses an exception with no project id and reports a refused write', async () => {
    serveDefaultListing();
    server.use(
      http.put(GRANT_PATTERN, () =>
        HttpResponse.json({ error: 'decide about this toolkit type first' }, { status: 409 }),
      ),
    );
    const user = userEvent.setup();
    renderAdminRoute(<AdminToolkitTypes />);

    await user.click(await screen.findByRole('button', { name: 'Projects' }));
    const dialog = await screen.findByRole('dialog');
    const add = within(dialog).getByRole('button', { name: 'Add exception' });
    expect(add).toBeDisabled();

    await user.type(within(dialog).getByLabelText(/Project ID/), '9');
    await user.type(within(dialog).getByLabelText(/Reason/), 'a reason');
    await user.click(add);

    expect(await screen.findByTestId('admin-toolkit-types-grant-error')).toHaveTextContent(
      'decide about this toolkit type first',
    );
  });
});

describe('AdminToolkitTypes bulk apply', () => {
  it('applies one decision to the types the filters are showing', async () => {
    serveDefaultListing();
    recordWrites();
    const user = userEvent.setup();
    renderAdminRoute(<AdminToolkitTypes />);
    expect(await screen.findByText('GitHub')).toBeInTheDocument();

    // Filter first, so the subject is provably the FILTERED set and not the
    // whole catalogue — a bulk control that quietly acted on everything is the
    // worst version of this feature.
    await user.type(await screen.findByLabelText('Search'), 'sql');
    await waitFor(() => {
      expect(screen.queryByText('GitHub')).not.toBeInTheDocument();
    });

    await user.click(screen.getByRole('button', { name: 'Apply to listed' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByTestId('admin-toolkit-types-bulk-subject')).toHaveTextContent(
      '1 types: sql',
    );

    const apply = within(dialog).getByRole('button', { name: 'Apply' });
    expect(apply).toBeDisabled();
    await user.type(within(dialog).getByLabelText(/Reason/), 'withdrawn for the audit');
    await user.click(apply);

    await waitFor(() => {
      expect(writes).toHaveLength(1);
    });
    expect(writes[0]!.method).toBe('POST');
    expect(writes[0]!.url).toContain('/admin/toolkit_types/administration/bulk');
    expect(writes[0]!.body).toEqual({
      types: ['sql'],
      availability: 'disabled',
      reason: 'withdrawn for the audit',
    });
  });

  it('reports a bulk apply that changed nothing rather than closing quietly', async () => {
    // The server answers 400 when NOTHING landed. A page that closed on it
    // would show a spinner and then no change, which reads as success.
    serveDefaultListing();
    server.use(
      http.post(BULK_PATTERN, () =>
        HttpResponse.json({ applied: [], failed: [{ type: 'sql' }] }, { status: 400 }),
      ),
    );
    const user = userEvent.setup();
    renderAdminRoute(<AdminToolkitTypes />);
    expect(await screen.findByText('GitHub')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Apply to listed' }));
    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByLabelText(/Reason/), 'a reason');
    await user.click(within(dialog).getByRole('button', { name: 'Apply' }));

    expect(await screen.findByTestId('admin-toolkit-types-bulk-error')).toBeInTheDocument();
  });
});
