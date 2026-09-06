/**
 * Rendering + write-path guard for the outbound e-mail editor (gap G7).
 *
 * The properties asserted here are the ones this screen's hazards make worth
 * asserting, and each one is invisible to a status-code test:
 *
 *  1. **The tri-state `password`.** Absent, `''` and a value mean leave it,
 *     clear it, and re-seal it. The form cannot echo the stored password, so a
 *     save that always sent the field would destroy the credential every time
 *     an operator corrected a host name — and every message after that would be
 *     refused by the relay at AUTH. This is the one bug here that is silent,
 *     permanent and impossible to notice from the UI, so the BODY of every
 *     write is inspected, not just its status.
 *  2. **No plaintext password is ever rendered.** The read carries
 *     `password_set` and there is no reveal, because nothing can reveal it.
 *  3. **The source tags.** A blank control is ambiguous on its own — "nobody
 *     set this" and "the chart sets it" look identical — and an operator who
 *     reads a blank host as "e-mail is off" on a working deployment is the
 *     failure this screen exists to prevent.
 *  4. **A refusal renders the SERVER's own sentence**, which on this surface
 *     names the field it refused.
 *
 * No fixture value here is or resembles a real credential.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminEmailEditor } from './AdminEmailEditor';
import { renderAdminRoute } from './__tests__/testRouter';

/** The stored layer names the host; the chart supplies the sender and origin. */
const STATE = {
  settings: {
    host: 'smtp.acme.example',
    port: 0,
    username: '',
    tls: '',
    from: '',
    reply_to: '',
    public_base_url: '',
  },
  effective: {
    host: 'smtp.acme.example',
    port: 587,
    username: '',
    tls: 'starttls',
    from: 'noreply@acme.example',
    reply_to: '',
    public_base_url: 'https://ai.acme.example',
  },
  sources: {
    host: 'database',
    port: 'environment',
    username: 'unset',
    tls: 'environment',
    from: 'environment',
    reply_to: 'unset',
    public_base_url: 'environment',
    password: 'database',
  },
  password_set: true,
  password_source: 'database',
  configured: true,
};

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

function useEmailHandlers(
  options: { state?: unknown; saveStatus?: number; saveBody?: Record<string, string> } = {},
): void {
  server.use(
    http.get('*/admin/email/administration', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json(options.state ?? STATE);
    }),
    http.put('*/admin/email/administration', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      if (options.saveStatus !== undefined) {
        return HttpResponse.json(options.saveBody, { status: options.saveStatus });
      }
      return HttpResponse.json(options.state ?? STATE);
    }),
    http.post('*/admin/email/test/administration', async ({ request }) => {
      recorded.push({ method: 'POST', url: request.url, body: await request.json() });
      return HttpResponse.json({ sent: true, to: 'ops@acme.example' });
    }),
  );
}

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method === 'PUT');
}

describe('AdminEmailEditor', () => {
  beforeEach(() => {
    recorded = [];
    configureGeneratedClient({ baseUrl: 'https://elitea.test/api/v2' });
  });
  afterEach(() => {
    resetGeneratedClient();
  });

  it('shows which layer decides each field and never a password', async () => {
    useEmailHandlers();
    renderAdminRoute(<AdminEmailEditor />);

    expect(await screen.findByTestId('admin-email-host')).toHaveValue('smtp.acme.example');

    // The tags. Without them, `from` renders blank on a deployment that sends
    // perfectly well, and the operator concludes e-mail is off.
    expect(screen.getByTestId('admin-email-source-host')).toHaveTextContent('Set here');
    expect(screen.getByTestId('admin-email-source-from')).toHaveTextContent('From the environment');
    expect(screen.getByTestId('admin-email-source-reply_to')).toHaveTextContent('Not set');

    // The inherited value is offered as the placeholder, so the operator can
    // see what they would be replacing.
    expect(screen.getByTestId('admin-email-from')).toHaveAttribute('placeholder', 'noreply@acme.example');

    // The password control is empty and there is no reveal.
    expect(screen.getByTestId('admin-email-password')).toHaveValue('');
    expect(screen.getByTestId('admin-email-password')).toHaveAttribute('type', 'password');
    expect(screen.getByTestId('admin-email-configured')).toBeInTheDocument();
  });

  it('omits the password from a save the operator did not touch it in', async () => {
    useEmailHandlers();
    const user = userEvent.setup({ delay: null });
    renderAdminRoute(<AdminEmailEditor />);

    // ONE keystroke, appended. Every field is controlled from one state
    // object, so each character re-renders the whole form; a cleared-and-
    // retyped host spends the test's budget on typing rather than on the
    // property below.
    await user.type(await screen.findByTestId('admin-email-host'), '2');
    await user.click(screen.getByTestId('admin-email-save'));

    await waitFor(() => expect(writes()).toHaveLength(1));
    const body = writes()[0]?.body as Record<string, unknown>;
    // THE assertion. A `password` key here — of any value — would have
    // overwritten or erased the sealed credential on an edit that had nothing
    // to do with it.
    expect(body).not.toHaveProperty('password');
    expect(body['host']).toBe('smtp.acme.example2');
  });

  it('sends an empty password only when the operator asked to remove it', async () => {
    useEmailHandlers();
    const user = userEvent.setup({ delay: null });
    renderAdminRoute(<AdminEmailEditor />);

    await screen.findByTestId('admin-email-host');
    await user.click(screen.getByTestId('admin-email-clear-password'));
    await user.click(screen.getByTestId('admin-email-save'));

    await waitFor(() => expect(writes()).toHaveLength(1));
    const body = writes()[0]?.body as Record<string, unknown>;
    expect(body['password']).toBe('');
  });

  it('sends a typed password so the server re-seals it', async () => {
    useEmailHandlers();
    const user = userEvent.setup({ delay: null });
    renderAdminRoute(<AdminEmailEditor />);

    await user.type(await screen.findByTestId('admin-email-password'), 'pw');
    await user.click(screen.getByTestId('admin-email-save'));

    await waitFor(() => expect(writes()).toHaveLength(1));
    const body = writes()[0]?.body as Record<string, unknown>;
    expect(body['password']).toBe('pw');
  });

  it('renders the server sentence when a save is refused', async () => {
    useEmailHandlers({
      saveStatus: 400,
      saveBody: { error: 'the port must be between 1 and 65535', field: 'port' },
    });
    const user = userEvent.setup({ delay: null });
    renderAdminRoute(<AdminEmailEditor />);

    await user.type(await screen.findByTestId('admin-email-port'), '70000');
    await user.click(screen.getByTestId('admin-email-save'));

    expect(await screen.findByTestId('admin-email-save-error')).toHaveTextContent(
      'the port must be between 1 and 65535',
    );
  });

  it('reports an unconfigured deployment with the reason the server gave', async () => {
    useEmailHandlers({
      state: {
        ...STATE,
        configured: false,
        reason: 'a sender address is required before e-mail can be sent: set From on Admin › E-mail, or EMAIL_FROM',
      },
    });
    renderAdminRoute(<AdminEmailEditor />);

    expect(await screen.findByTestId('admin-email-not-configured')).toHaveTextContent(
      'a sender address is required',
    );
  });

  it('sends the test message to the address the operator typed', async () => {
    useEmailHandlers();
    const user = userEvent.setup({ delay: null });
    renderAdminRoute(<AdminEmailEditor />);

    await user.type(await screen.findByTestId('admin-email-test-to'), 'a@b.example');
    await user.click(screen.getByTestId('admin-email-test-send'));

    await waitFor(() => {
      const posts = recorded.filter((entry) => entry.method === 'POST');
      expect(posts).toHaveLength(1);
      expect(posts[0]?.body).toEqual({ to: 'a@b.example' });
    });
  });
});
