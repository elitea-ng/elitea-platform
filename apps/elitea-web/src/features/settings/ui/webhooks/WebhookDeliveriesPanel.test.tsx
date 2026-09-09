/**
 * WebhookDeliveriesPanel.test.tsx (#876's second half) — pins the "Recent
 * deliveries" panel: it renders the rows a real GET .../deliveries answers,
 * and its Redeliver button issues the real POST and refreshes the list.
 *
 * Same MSW-over-the-generated-client technique
 * RequestModelConnection.test.tsx uses, rather than mocking the generated
 * module directly: a mocked module would keep passing if the component
 * stopped calling the real endpoint at all.
 */
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { server } from '../../../../test/setup';

import { WebhookDeliveriesPanel } from './WebhookDeliveriesPanel';

const BASE = '/api/v2';
const PROJECT_ID = '7';
const WEBHOOK_ID = 'wh-1';

let redeliverCalls: string[];
/**
 * Mutable so the POST handler can APPEND to what the GET handler reads:
 * `invalidateQueries` triggers exactly one automatic refetch after the
 * mutation resolves, and that refetch must see the row the redeliver just
 * created — a static mock would leave the refreshed list identical to the
 * one before the click, and the assertion would pass for the wrong reason
 * (a stale cache the panel never actually re-queried).
 */
let currentItems: unknown[];

function mockDeliveries(items: unknown[]): void {
  currentItems = items;
  server.use(
    http.get(`*/webhooks/prompt_lib/${PROJECT_ID}/${WEBHOOK_ID}/deliveries`, () =>
      HttpResponse.json({ items: currentItems }, { status: 200 }),
    ),
  );
}

function mockRedeliver(buildNewDelivery: (deliveryId: string) => Record<string, unknown>): void {
  server.use(
    http.post(`*/webhooks/prompt_lib/${PROJECT_ID}/${WEBHOOK_ID}/deliveries/:deliveryId/redeliver`, ({ params }) => {
      const deliveryId = String(params.deliveryId);
      redeliverCalls.push(deliveryId);
      const newDelivery = buildNewDelivery(deliveryId);
      currentItems = [...currentItems, newDelivery];
      return HttpResponse.json(newDelivery, { status: 201 });
    }),
  );
}

function renderPanel(canRedeliver = true): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <CssBaseline />
        <WebhookDeliveriesPanel projectId={PROJECT_ID} webhookId={WEBHOOK_ID} canRedeliver={canRedeliver} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  redeliverCalls = [];
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

const successDelivery = {
  id: 'del-1',
  webhook_id: WEBHOOK_ID,
  project_id: PROJECT_ID,
  event: 'conversation.created',
  status: 'success',
  attempts: 1,
  response_code: 200,
  last_error: '',
  payload: { type: 'conversation.created' },
  redelivery_of: '',
  created_at: '2026-09-01T12:00:00Z',
  updated_at: '2026-09-01T12:00:00Z',
};

const failedDelivery = {
  id: 'del-2',
  webhook_id: WEBHOOK_ID,
  project_id: PROJECT_ID,
  event: 'artifact.uploaded',
  status: 'failed',
  attempts: 3,
  response_code: 500,
  last_error: 'destination responded 500',
  payload: { type: 'artifact.uploaded' },
  redelivery_of: '',
  created_at: '2026-09-01T13:00:00Z',
  updated_at: '2026-09-01T13:00:00Z',
};

describe('WebhookDeliveriesPanel', () => {
  it('renders an empty state when the webhook has no deliveries yet', async () => {
    mockDeliveries([]);
    renderPanel();

    await screen.findByText(/No deliveries yet/i);
  });

  it('renders a real delivery row with its event, status, attempts and response code', async () => {
    mockDeliveries([successDelivery]);
    renderPanel();

    const row = await screen.findByTestId(`webhook-delivery-${successDelivery.id}`);
    expect(within(row).getByText('conversation.created')).toBeInTheDocument();
    expect(within(row).getByText('success')).toBeInTheDocument();
    expect(within(row).getByText('1')).toBeInTheDocument();
    expect(within(row).getByText('200')).toBeInTheDocument();
  });

  it('surfaces the last_error on a failed delivery', async () => {
    mockDeliveries([failedDelivery]);
    renderPanel();

    const row = await screen.findByTestId(`webhook-delivery-${failedDelivery.id}`);
    expect(within(row).getByText('failed')).toBeInTheDocument();
    expect(within(row).getByText('destination responded 500')).toBeInTheDocument();
  });

  it('hides the Redeliver action when the caller lacks the update permission', async () => {
    mockDeliveries([successDelivery]);
    renderPanel(false);

    await screen.findByTestId(`webhook-delivery-${successDelivery.id}`);
    expect(screen.queryByTestId(`webhook-redeliver-${successDelivery.id}`)).not.toBeInTheDocument();
  });

  it('Redeliver calls the real endpoint and the refetch shows the new linked row', async () => {
    mockDeliveries([successDelivery]);
    mockRedeliver((deliveryId) => ({ ...successDelivery, id: 'del-3', redelivery_of: deliveryId }));
    renderPanel();

    await screen.findByTestId(`webhook-delivery-${successDelivery.id}`);
    await userEvent.click(screen.getByTestId(`webhook-redeliver-${successDelivery.id}`));

    await waitFor(() => expect(redeliverCalls).toEqual([successDelivery.id]));

    // invalidateQueries' automatic refetch is what surfaces del-3, not a
    // client-side splice of the mutation's own response — see
    // mockRedeliver's comment on why currentItems is shared with the GET
    // handler.
    await screen.findByTestId('webhook-delivery-del-3');
    expect(screen.getByText(`Redelivery of ${successDelivery.id}`)).toBeInTheDocument();
  });
});
