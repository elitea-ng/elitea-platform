/**
 * Rendering and behaviour tests for `pages/admin/ServiceDescriptors.tsx` (unit A14).
 *
 * This page is deliberately unavailable, so "it renders" is close to worthless as
 * a bar. Each test asserts one of the properties that make the unavailability
 * honest rather than decorative:
 *
 *  - the sentence shown is the SERVER's, not one the page carries — the test
 *    serves a reason no source file contains and expects to see it;
 *  - a 501 and a 500 are told apart, so a broken server cannot pass itself off as
 *    a considered architectural decision;
 *  - if the server ever answers 200 WITH ROWS, the page shows them and drops the
 *    notice. That is the assertion that would catch someone wiring the endpoint
 *    to a stub, which is the failure this whole unit exists to remove;
 *  - the REQUEST is the one the legacy API served — the right path, the right
 *    mode, and GET only. No registration verb is ever issued, because no
 *    control exists to issue one.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminServiceDescriptors } from './ServiceDescriptors';
import { renderAdminRoute } from './__tests__/testRouter';

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
}

let recorded: RecordedRequest[] = [];

/**
 * A sentence that appears in NO source file of this app. If the page can show
 * it, the page is showing what the server said.
 */
const SERVER_REASON =
  'the provider hub is not part of this deployment: nothing stores a descriptor and nothing looks one up';

const DESCRIPTORS_PATH = '*/elitea_core/admin/administration';

function respondWith(status: number, body: Record<string, unknown>): void {
  server.use(
    http.get(DESCRIPTORS_PATH, ({ request }) => {
      recorded.push({ method: request.method, url: request.url });
      return HttpResponse.json(body, { status });
    }),
    // Any registration verb reaching the network is a failure of this page, so
    // it is recorded rather than answered plausibly.
    http.post('*/elitea_core/register_descriptor/*', ({ request }) => {
      recorded.push({ method: request.method, url: request.url });
      return HttpResponse.json({ error: 'unexpected' }, { status: 501 });
    }),
    http.delete('*/elitea_core/register_descriptor/*', ({ request }) => {
      recorded.push({ method: request.method, url: request.url });
      return HttpResponse.json({ error: 'unexpected' }, { status: 501 });
    }),
  );
}

/**
 * One inactive row with a digest — the shape an operator can act on.
 *
 * `published_manifest_digest` is not decoration in this fixture: the Activate
 * control is only offered for a row that has one, because the request asserts
 * it. A fixture without it would render no button and every assertion below
 * would fail for the wrong reason.
 */
const INACTIVE_ROW = {
  project_id: 41,
  provider_name: 'wikis',
  service_location_url: 'https://elitea-deepwiki:8080',
  healthy: true,
  status: 'inactive',
  reason: 'recorded, not in force',
  published_manifest_digest: 'a'.repeat(64),
};

interface RecordedWrite {
  readonly url: string;
  readonly body: Record<string, unknown>;
}

/**
 * Serves the listing and captures what the activate route was actually sent.
 * `activateStatus` lets one test drive the 422 path — the outcome the dialog is
 * built around.
 */
function serveActivatable(
  writes: RecordedWrite[],
  options: { readonly rows?: readonly Record<string, unknown>[]; readonly activateStatus?: number } = {},
): void {
  const rows = options.rows ?? [INACTIVE_ROW];
  server.use(
    http.get(DESCRIPTORS_PATH, () =>
      HttpResponse.json({ rows, total: rows.length, admission_posture: 'record' }, { status: 200 }),
    ),
    http.post('*/elitea_core/register_descriptor/*/activate', async ({ request }) => {
      writes.push({ url: request.url, body: (await request.json()) as Record<string, unknown> });
      const status = options.activateStatus ?? 200;
      if (status !== 200) {
        return HttpResponse.json(
          { error: 'the manifest changed since it was reviewed' },
          { status },
        );
      }
      return HttpResponse.json({ status: 'active' }, { status: 200 });
    }),
    http.post('*/elitea_core/register_descriptor/*/deactivate', ({ request }) => {
      writes.push({ url: request.url, body: {} });
      return HttpResponse.json({ status: 'inactive' }, { status: 200 });
    }),
  );
}

describe('AdminServiceDescriptors', () => {
  beforeEach(() => {
    recorded = [];
    configureGeneratedClient({ baseUrl: '/api/v2' });
  });

  afterEach(() => {
    resetGeneratedClient();
  });

  it('renders the reason the SERVER gave, not one of its own', async () => {
    respondWith(501, { error: SERVER_REASON });

    renderAdminRoute(<AdminServiceDescriptors />);

    const notice = await screen.findByTestId('admin-service-descriptors-unavailable');
    expect(notice).toHaveTextContent(SERVER_REASON);
    // And ONLY the notice. A 501 is not additionally a load failure, and showing
    // both would tell the operator the explanation might itself be a symptom.
    expect(screen.queryByTestId('admin-service-descriptors-error')).toBeNull();
  });

  it('asks the legacy endpoint, in administration mode, with GET only', async () => {
    respondWith(501, { error: SERVER_REASON });

    renderAdminRoute(<AdminServiceDescriptors />);
    await screen.findByTestId('admin-service-descriptors-unavailable');

    expect(recorded).toHaveLength(1);
    expect(recorded[0]?.method).toBe('GET');
    expect(recorded[0]?.url).toContain('/elitea_core/admin/administration');
    // The reference client also declares a delete. This page must not have one.
    expect(recorded.some((entry) => entry.method !== 'GET')).toBe(false);
  });

  it('does not present a failed request as a considered architectural decision', async () => {
    respondWith(500, { error: 'database is on fire' });

    renderAdminRoute(<AdminServiceDescriptors />);

    await screen.findByTestId('admin-service-descriptors-error');
    expect(screen.queryByTestId('admin-service-descriptors-unavailable')).toBeNull();
  });

  it('shows rows and drops the notice if the endpoint ever answers with them', async () => {
    // The regression guard: a stub answering 200 must be VISIBLE, not silently
    // rendered as an empty page still displaying the unavailability text.
    respondWith(200, {
      total: 2,
      rows: [
        {
          project_id: 13,
          provider_name: 'deepwiki',
          service_location_url: 'http://provider.example.com/deepwiki',
          healthy: true,
        },
        {
          project_id: 13,
          provider_name: 'inventory',
          service_location_url: 'http://provider.example.com/inventory',
          healthy: false,
        },
      ],
    });

    renderAdminRoute(<AdminServiceDescriptors />);

    expect(await screen.findByText('deepwiki')).toBeVisible();
    expect(screen.getByText('inventory')).toBeVisible();
    expect(screen.getByText('http://provider.example.com/deepwiki')).toBeVisible();
    // Both health states in one render. Asserting only the unhealthy one — as
    // the single-row test below does — leaves the healthy label free to change
    // to "No" without any test noticing, and two rows both reading "No" is a
    // listing that reports a working provider as broken.
    expect(screen.getByText('Yes')).toBeVisible();
    expect(screen.getByText('No')).toBeVisible();
    expect(screen.queryByTestId('admin-service-descriptors-unavailable')).toBeNull();
    expect(screen.queryByTestId('admin-service-descriptors-error')).toBeNull();
  });

  it('distinguishes an unhealthy provider from a healthy one', async () => {
    respondWith(200, {
      rows: [
        {
          project_id: 1,
          provider_name: 'imagegen',
          service_location_url: 'https://provider.example.com/imagegen',
          healthy: false,
        },
      ],
    });

    renderAdminRoute(<AdminServiceDescriptors />);

    // `healthy` is a boolean and rendering it verbatim would print "false",
    // which reads as a value rather than as a state.
    // The colour is not asserted here, and does not need to be: R-T6 forbids
    // asserting on MUI's internal class names, and `ServiceDescriptorsTable`
    // selects the colour and the label together from one constant, so they
    // cannot disagree. The label is what a screen reader gets either way.
    expect(await screen.findByText('No')).toBeVisible();
    expect(screen.queryByText('Yes')).toBeNull();
  });

  it('shows a REVOKED provider as revoked rather than as one more live row', async () => {
    // The listing is driven by every origin ever registered, and DELETE revokes
    // instead of deleting, so a revoked provider STAYS in the table. Without an
    // admission column it stays there looking exactly like a live one — and an
    // operator who revokes it, reloads, and sees no change concludes the revoke
    // did not work.
    respondWith(200, {
      rows: [
        {
          project_id: 3,
          provider_name: 'retired',
          service_location_url: 'https://provider.example.com/retired',
          healthy: null,
          status: 'revoked',
          reason: 'superseded',
        },
        {
          project_id: 3,
          provider_name: 'recorded',
          service_location_url: 'https://provider.example.com/recorded',
          healthy: true,
          status: 'inactive',
          reason: 'no admission overlay',
        },
      ],
    });

    renderAdminRoute(<AdminServiceDescriptors />);

    expect(await screen.findByText('Revoked')).toBeVisible();
    expect(screen.getByText('Inactive')).toBeVisible();
    // Both rows are still listed; the revoke did not remove one.
    expect(screen.getByText('retired')).toBeVisible();
    expect(screen.getByText('recorded')).toBeVisible();
  });

  it('reads a row with no admission as not registered', async () => {
    // An origin with no admitted revision is a real state, and the one an
    // operator is usually looking for. The server sends `unregistered`.
    respondWith(200, {
      rows: [
        {
          project_id: 3,
          provider_name: 'origin-only',
          service_location_url: 'https://provider.example.com/origin-only',
          healthy: null,
          status: 'unregistered',
        },
      ],
    });

    renderAdminRoute(<AdminServiceDescriptors />);
    expect(await screen.findByText('Not registered')).toBeVisible();
  });

  it('shows an admission state this build does not know VERBATIM', async () => {
    // The plane can gain a state before this page does. Mapping an unfamiliar
    // one onto a default would be a confident wrong answer where the raw word
    // is a correct one.
    respondWith(200, {
      rows: [
        {
          project_id: 3,
          provider_name: 'future',
          service_location_url: 'https://provider.example.com/future',
          healthy: null,
          status: 'quarantined',
        },
      ],
    });

    renderAdminRoute(<AdminServiceDescriptors />);
    expect(await screen.findByText('quarantined')).toBeVisible();
    expect(screen.queryByText('Not registered')).toBeNull();
  });

  it('reports an unprobed provider as Unknown, not as unhealthy', async () => {
    // THE THREE-STATE CONTRACT, and the one state the legacy runtime could not
    // express. `provider_health_projection` is a separate table with its own
    // timestamp, and the server answers `null` when no projection is fresh
    // enough to make a claim. Rendering that as "No" reports a provider nobody
    // has asked about as broken — the exact defect the projection table was
    // split out to stop.
    //
    // Both other rows are here on purpose: with only the null row, a component
    // that had collapsed to a single chip would still pass.
    respondWith(200, {
      rows: [
        {
          project_id: 4,
          provider_name: 'unprobed',
          service_location_url: 'https://provider.example.com/unprobed',
          healthy: null,
        },
        {
          project_id: 4,
          provider_name: 'live',
          service_location_url: 'https://provider.example.com/live',
          healthy: true,
        },
        {
          project_id: 4,
          provider_name: 'down',
          service_location_url: 'https://provider.example.com/down',
          healthy: false,
        },
      ],
    });

    renderAdminRoute(<AdminServiceDescriptors />);

    expect(await screen.findByText('Unknown')).toBeVisible();
    expect(screen.getByText('Yes')).toBeVisible();
    expect(screen.getByText('No')).toBeVisible();
    // Exactly one of each: three rows must not render three copies of one
    // label, which is what a state that fell through to a default would do.
    expect(screen.getAllByText('Unknown')).toHaveLength(1);
    expect(screen.getAllByText('No')).toHaveLength(1);
  });

  it('treats a descriptor with no healthy field at all as Unknown', async () => {
    // An absent field says exactly as little as an explicit null. This is the
    // repo's recurring "absence reads as correctness" shape pointed the other
    // way: absence must read as NO CLAIM, never as a false one.
    respondWith(200, {
      rows: [
        {
          project_id: 7,
          provider_name: 'silent',
          service_location_url: 'https://provider.example.com/silent',
        },
      ],
    });

    renderAdminRoute(<AdminServiceDescriptors />);

    expect(await screen.findByText('Unknown')).toBeVisible();
    expect(screen.queryByText('No')).toBeNull();
  });

  it('renders an empty listing without claiming the surface is unavailable', async () => {
    // "Nothing is registered" and "this platform has no provider hub" are
    // different facts, and the second one must come from the server.
    respondWith(200, { total: 0, rows: [] });

    const { queryClient } = renderAdminRoute(<AdminServiceDescriptors />);

    await waitFor(() => {
      expect(screen.getByText('No service descriptors are registered.')).toBeVisible();
    });
    expect(screen.queryByTestId('admin-service-descriptors-unavailable')).toBeNull();

    // The listing lands under the namespace this module declares. A key built
    // somewhere else is a cache nothing else can reach or invalidate — the
    // read/write namespace split that made saved data look absent in #132.
    expect(queryClient.getQueryData(['admin', 'service-descriptors', 'list'])).toEqual({
      rows: [],
      posture: undefined,
    });
  });

  it('does not read `rows` off the transport envelope', async () => {
    // #132: `eliteaFetch` resolves the envelope, not the body. A client peeling
    // it by hand — or forgetting to — would render "no descriptors registered"
    // for a response that carries two, which on this page is worse than empty:
    // it is a wrong answer that looks like a right one.
    respondWith(200, {
      rows: [
        {
          project_id: 7,
          provider_name: 'envelope-probe',
          service_location_url: 'https://provider.example.com/probe',
          healthy: true,
        },
      ],
    });

    renderAdminRoute(<AdminServiceDescriptors />);

    expect(await screen.findByText('envelope-probe')).toBeVisible();
    expect(screen.queryByText('No service descriptors are registered.')).toBeNull();
  });
});

describe('AdminServiceDescriptors activation (migration 0109)', () => {
  beforeEach(() => {
    recorded = [];
    configureGeneratedClient({ baseUrl: '/api/v2' });
  });

  afterEach(() => {
    resetGeneratedClient();
  });

  it('sends the ROW\'s digest as expected_digest, with the operator\'s reason', async () => {
    // THE ASSERTION THE COMPARE-AND-SWAP DEPENDS ON. The request must assert
    // the digest the operator was looking at. A digest re-read at click time
    // would agree with whatever the provider had just published — which is the
    // case the server's 422 exists to catch, and would make it unreachable.
    const writes: RecordedWrite[] = [];
    serveActivatable(writes);

    const user = userEvent.setup();
    renderAdminRoute(<AdminServiceDescriptors />);

    await user.click(await screen.findByRole('button', { name: 'Activate' }));
    await user.type(await screen.findByLabelText(/Reason/), 'reviewed the wikis toolkit');
    await user.click(screen.getByRole('button', { name: 'Activate', hidden: false }));

    await waitFor(() => {
      expect(writes).toHaveLength(1);
    });
    expect(writes[0]?.url).toContain('/register_descriptor/41/activate');
    expect(writes[0]?.url).toContain('provider_name=wikis');
    expect(writes[0]?.body.expected_digest).toBe('a'.repeat(64));
    expect(writes[0]?.body.reason).toBe('reviewed the wikis toolkit');
  });

  it('will not submit an activation with no reason', async () => {
    // The server refuses a reasonless activation with 400. The dialog refuses
    // it before the request, so the operator sees the rule rather than a
    // failure — and a request that never leaves is the proof.
    const writes: RecordedWrite[] = [];
    serveActivatable(writes);

    const user = userEvent.setup();
    renderAdminRoute(<AdminServiceDescriptors />);

    await user.click(await screen.findByRole('button', { name: 'Activate' }));
    const confirm = await screen.findByRole('button', { name: 'Activate', hidden: false });
    expect(confirm).toBeDisabled();
    // Whitespace is not a reason.
    await user.type(await screen.findByLabelText(/Reason/), '   ');
    expect(confirm).toBeDisabled();
    expect(writes).toHaveLength(0);
  });

  it('keeps the dialog open and shows the SERVER\'s message when activation is refused', async () => {
    // The 422 is the interesting outcome: the provider republished between the
    // review and the click. Closing the dialog would replace that message with
    // a row that silently did not change.
    const writes: RecordedWrite[] = [];
    serveActivatable(writes, { activateStatus: 422 });

    const user = userEvent.setup();
    renderAdminRoute(<AdminServiceDescriptors />);

    await user.click(await screen.findByRole('button', { name: 'Activate' }));
    await user.type(await screen.findByLabelText(/Reason/), 'reviewed');
    await user.click(screen.getByRole('button', { name: 'Activate', hidden: false }));

    const failure = await screen.findByTestId('admin-service-descriptors-write-error');
    expect(failure).toHaveTextContent('the manifest changed since it was reviewed');
    // Still open, still holding what was typed.
    expect(await screen.findByLabelText(/Reason/)).toHaveValue('reviewed');
  });

  it('offers Deactivate on an active row and Activate on nothing else', async () => {
    // `revoked` is terminal and `unregistered` has no revision. A control on
    // either would be a button whose only outcome is a refusal.
    serveActivatable([], {
      rows: [
        { ...INACTIVE_ROW, provider_name: 'live', status: 'active' },
        { ...INACTIVE_ROW, provider_name: 'retired', status: 'revoked' },
        { ...INACTIVE_ROW, provider_name: 'origin-only', status: 'unregistered' },
      ],
    });

    renderAdminRoute(<AdminServiceDescriptors />);

    expect(await screen.findByRole('button', { name: 'Deactivate' })).toBeVisible();
    expect(screen.queryByRole('button', { name: 'Activate' })).toBeNull();
  });

  it('offers no Activate for a row with no manifest digest', async () => {
    // Nothing to assert, so nothing to activate. Rendering the control would
    // send an empty expected_digest and collect a 400.
    serveActivatable([], {
      rows: [{ ...INACTIVE_ROW, published_manifest_digest: null }],
    });

    renderAdminRoute(<AdminServiceDescriptors />);
    await screen.findByText('wikis');
    expect(screen.queryByRole('button', { name: 'Activate' })).toBeNull();
  });

  it('states the posture, because `inactive` means two different things without it', async () => {
    serveActivatable([]);
    renderAdminRoute(<AdminServiceDescriptors />);
    expect(await screen.findByTestId('admin-admission-posture')).toHaveTextContent(
      'Admission recorded only',
    );
  });

  it('says the deployment is enforcing when it is', async () => {
    server.use(
      http.get(DESCRIPTORS_PATH, () =>
        HttpResponse.json({ rows: [INACTIVE_ROW], admission_posture: 'enforce' }, { status: 200 }),
      ),
    );
    renderAdminRoute(<AdminServiceDescriptors />);
    expect(await screen.findByTestId('admin-admission-posture')).toHaveTextContent(
      'Admission in force',
    );
  });

  it('says NOTHING about the posture when the server sent none', async () => {
    // A guessed `record` would be a reassuring word on a deployment that might
    // be enforcing — the "absence reads as correctness" shape, pointed at the
    // operator.
    server.use(
      http.get(DESCRIPTORS_PATH, () => HttpResponse.json({ rows: [INACTIVE_ROW] }, { status: 200 })),
    );
    renderAdminRoute(<AdminServiceDescriptors />);
    await screen.findByText('wikis');
    expect(screen.queryByTestId('admin-admission-posture')).toBeNull();
  });
});
