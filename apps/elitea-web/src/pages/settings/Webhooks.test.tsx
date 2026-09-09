/**
 * Coverage for `WebhooksContent` (#876) — the outbound project webhook
 * registry's first UI. Before this issue the only surface was the API; these
 * tests exercise the real generated client through MSW, the same harness
 * `Secrets.test.tsx` uses for the sibling settings tab.
 */
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { server } from '@/test/setup';

import { WebhooksContent } from './Webhooks';

const BASE = '/api/v2';
const PERMISSIONS_PATH = `${BASE}/auth/permissions/prompt_lib/:projectId`;
const WEBHOOKS_PATH = `${BASE}/webhooks/prompt_lib/:projectID`;
const WEBHOOK_PATH = `${BASE}/webhooks/prompt_lib/:projectID/:webhookID`;

function fullPermissions(): ReturnType<typeof http.get> {
  return http.get(PERMISSIONS_PATH, () =>
    HttpResponse.json([
      { name: 'configurations.configurations.list', enabled: true },
      { name: 'configurations.configuration.details', enabled: true },
      { name: 'configurations.configuration.create', enabled: true },
      { name: 'configurations.configuration.update', enabled: true },
      { name: 'configurations.configuration.delete', enabled: true },
    ]),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  useSelectedProjectStore.setState({ project: { id: 'proj-1', name: 'Acme' } });
});

afterEach(() => {
  resetGeneratedClient();
  useSelectedProjectStore.setState({ project: null });
});

function mount() {
  return render(
    <AppProviders>
      <WebhooksContent />
    </AppProviders>,
  );
}

describe('WebhooksContent — happy path', () => {
  it('renders the fetched webhooks as rows', async () => {
    server.use(
      fullPermissions(),
      http.get(WEBHOOKS_PATH, () =>
        HttpResponse.json({
          items: [
            {
              id: 'wh-1',
              project_id: 'proj-1',
              url: 'https://example.com/hook',
              events: ['application.created'],
              secret: 'sec-1',
              active: true,
              created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-01-01T00:00:00Z',
            },
          ],
        }),
      ),
    );

    mount();

    await waitFor(() => {
      expect(screen.getByText('https://example.com/hook')).toBeInTheDocument();
    });
    expect(screen.getByText('application.created')).toBeInTheDocument();
  });

  it('shows an empty state, not a permanent skeleton, for a project with no webhooks', async () => {
    let listCalls = 0;
    server.use(
      fullPermissions(),
      http.get(WEBHOOKS_PATH, () => {
        listCalls += 1;
        return HttpResponse.json({ items: [] });
      }),
    );

    mount();

    // Wait for the LIST REQUEST ITSELF, not the empty-state markup — that
    // markup also renders (indistinguishably) while the permission query is
    // still pending and the list query is therefore disabled, which is a
    // false "empty" a real deployment never shows.
    await waitFor(() => {
      expect(listCalls).toBeGreaterThan(0);
    });
    await waitFor(() => {
      expect(screen.getByTestId('webhooks-empty')).toBeInTheDocument();
    });
  });
});

describe('WebhooksContent — create', () => {
  it('creates a webhook with a client-generated secret and reveals it once', async () => {
    let createdBody: { url: string; events: string[]; secret: string; active: boolean } | undefined;
    let listCalls = 0;
    server.use(
      fullPermissions(),
      http.get(WEBHOOKS_PATH, () => {
        listCalls += 1;
        return HttpResponse.json({ items: [] });
      }),
      http.post(WEBHOOKS_PATH, async ({ request }) => {
        createdBody = (await request.json()) as typeof createdBody;
        return HttpResponse.json(
          {
            id: 'wh-new',
            project_id: 'proj-1',
            url: createdBody!.url,
            events: createdBody!.events,
            secret: createdBody!.secret,
            active: createdBody!.active,
            created_at: '2026-01-01T00:00:00Z',
            updated_at: '2026-01-01T00:00:00Z',
          },
          { status: 201 },
        );
      }),
    );

    mount();

    await waitFor(() => {
      expect(listCalls).toBeGreaterThan(0);
    });
    await waitFor(() => {
      expect(screen.getByTestId('webhooks-empty')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole('button', { name: 'Register a new webhook' }));

    const dialog = await screen.findByTestId('webhook-form-dialog');
    fireEvent.change(within(dialog).getByTestId('webhook-form-url').querySelector('input')!, {
      target: { value: 'https://example.com/webhooks/elitea' },
    });
    fireEvent.change(within(dialog).getByTestId('webhook-form-events').querySelector('input')!, {
      target: { value: 'application.created, execution.completed' },
    });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create' }));

    await waitFor(() => {
      expect(createdBody).toBeDefined();
    });
    expect(createdBody!.url).toBe('https://example.com/webhooks/elitea');
    expect(createdBody!.events).toEqual(['application.created', 'execution.completed']);
    expect(createdBody!.active).toBe(true);
    expect(createdBody!.secret.length).toBeGreaterThan(0);

    // The secret dialog shows the exact value the POST sent, once, right
    // after creation — the reveal-once UX the issue asks for.
    await waitFor(() => {
      expect(screen.getByTestId('webhook-secret-value')).toHaveValue(createdBody!.secret);
    });
  });
});

describe('WebhooksContent — enable/disable and rotate', () => {
  it('toggling active sends the full record with only `active` flipped', async () => {
    const updates: { webhookID: string; body: unknown }[] = [];
    server.use(
      fullPermissions(),
      http.get(WEBHOOKS_PATH, () =>
        HttpResponse.json({
          items: [
            {
              id: 'wh-1',
              project_id: 'proj-1',
              url: 'https://example.com/hook',
              events: ['application.created'],
              secret: 'sec-1',
              active: true,
              created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-01-01T00:00:00Z',
            },
          ],
        }),
      ),
      http.put(WEBHOOK_PATH, async ({ request, params }) => {
        updates.push({ webhookID: String(params.webhookID), body: await request.json() });
        return HttpResponse.json({});
      }),
    );

    mount();

    await waitFor(() => {
      expect(screen.getByTestId('webhook-active-toggle-wh-1')).toBeInTheDocument();
    });
    // MUI's `Switch` forwards `data-testid` to the root `<span>`; the actual
    // toggle is the nested `<input type="checkbox">`.
    fireEvent.click(within(screen.getByTestId('webhook-active-toggle-wh-1')).getByRole('switch'));

    await waitFor(() => {
      expect(updates).toHaveLength(1);
    });
    expect(updates[0]!.webhookID).toBe('wh-1');
    expect(updates[0]!.body).toEqual({
      url: 'https://example.com/hook',
      events: ['application.created'],
      secret: 'sec-1',
      active: false,
    });
  });

  it('rotating mints a new secret and reveals it once', async () => {
    const updates: { body: { secret: string } }[] = [];
    server.use(
      fullPermissions(),
      http.get(WEBHOOKS_PATH, () =>
        HttpResponse.json({
          items: [
            {
              id: 'wh-1',
              project_id: 'proj-1',
              url: 'https://example.com/hook',
              events: ['application.created'],
              secret: 'sec-1',
              active: true,
              created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-01-01T00:00:00Z',
            },
          ],
        }),
      ),
      http.put(WEBHOOK_PATH, async ({ request }) => {
        const body = (await request.json()) as { secret: string };
        updates.push({ body });
        return HttpResponse.json({});
      }),
    );

    mount();

    await waitFor(() => {
      expect(screen.getByTestId('webhook-rotate-wh-1')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByTestId('webhook-rotate-wh-1'));

    await waitFor(() => {
      expect(updates).toHaveLength(1);
    });
    expect(updates[0]!.body.secret).not.toBe('sec-1');
    await waitFor(() => {
      expect(screen.getByTestId('webhook-secret-value')).toHaveValue(updates[0]!.body.secret);
    });
  });
});
