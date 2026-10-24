/**
 * The Tools pricing tab (#1238): the metered operations' per-unit prices,
 * saved through PUT /admin/gateway/metered-prices.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { LlmProxyToolPricesPanel } from './LlmProxyToolPricesPanel';
import { renderAdminRoute } from './__tests__/testRouter';

const PRICES = {
  currency: 'USD',
  prices: [
    { provider: 'tavily', operation: 'web_search', unit: 'credit', price_per_unit_usd: '0.008', price_overridden: false },
    { provider: 'ddg_instant', operation: 'web_search', unit: 'request', price_per_unit_usd: '0', price_overridden: true },
    { provider: 'shodan', operation: 'host_lookup', unit: 'credit', price_per_unit_usd: '0.0000005', price_overridden: true },
  ],
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('LlmProxyToolPricesPanel', () => {
  it('lists each operation with its unit and saves an edited price', async () => {
    let saved: unknown;
    server.use(
      http.get('*/admin/gateway/metered-prices', () => HttpResponse.json(PRICES)),
      http.put('*/admin/gateway/metered-prices', async ({ request }) => {
        saved = await request.json();
        return HttpResponse.json({ ...PRICES.prices[0], price_per_unit_usd: '0.01', price_overridden: true });
      }),
    );
    const user = userEvent.setup();
    renderAdminRoute(<LlmProxyToolPricesPanel />);

    const row = await screen.findByTestId('metered-price-tavily-web_search');
    expect(row).toHaveTextContent('Web search');
    expect(row).toHaveTextContent('Tavily');
    expect(row).toHaveTextContent('per credit');
    expect(screen.getByTestId('metered-price-ddg_instant-web_search')).toHaveTextContent('Set by an administrator');

    const input = within(row).getByRole('textbox');
    await user.clear(input);
    await user.type(input, '0.01');
    await user.click(within(row).getByRole('button', { name: 'Save' }));

    await expect.poll(() => saved).toEqual({
      provider: 'tavily',
      operation: 'web_search',
      unit: 'credit',
      price_per_unit_usd: '0.01',
    });
  });

  it('shows and sends a sub-micro price as a decimal string, never an exponent', async () => {
    let saved: unknown;
    server.use(
      http.get('*/admin/gateway/metered-prices', () => HttpResponse.json(PRICES)),
      http.put('*/admin/gateway/metered-prices', async ({ request }) => {
        saved = await request.json();
        return HttpResponse.json({ ...PRICES.prices[2], price_per_unit_usd: '0.0000004' });
      }),
    );
    const user = userEvent.setup();
    renderAdminRoute(<LlmProxyToolPricesPanel />);

    const row = await screen.findByTestId('metered-price-shodan-host_lookup');
    const input = within(row).getByRole('textbox');
    expect(input).toHaveValue('0.0000005');
    await user.clear(input);
    await user.type(input, '0.0000004');
    await user.click(within(row).getByRole('button', { name: 'Save' }));

    await expect.poll(() => saved).toEqual({
      provider: 'shodan',
      operation: 'host_lookup',
      unit: 'credit',
      price_per_unit_usd: '0.0000004',
    });
  });

  it('says every tool call is refused when nothing is priced', async () => {
    server.use(http.get('*/admin/gateway/metered-prices', () => HttpResponse.json({ currency: 'USD', prices: [] })));
    renderAdminRoute(<LlmProxyToolPricesPanel />);

    expect(await screen.findByTestId('metered-prices-empty')).toBeVisible();
  });
});
