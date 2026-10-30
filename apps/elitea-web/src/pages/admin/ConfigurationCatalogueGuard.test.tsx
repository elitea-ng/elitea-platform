/**
 * The `catalogue` section warns before switching to approved_only while
 * connectors in use have no approved request (ADR-0044 decision 1, #1501):
 * the count comes from the server's inventory, the save asks first, and
 * with nothing unapproved it saves at once.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminConfiguration } from './Configuration';
import { renderAdminRoute } from './__tests__/testRouter';

const SECTION = 'catalogue';

const CATALOGUE_SECTION = {
  id: SECTION,
  title: 'Connector Catalogue',
  description: 'Which toolkits and MCP servers projects may use.',
  always_visible: true,
  fields: [
    { key: 'mode', type: 'string', title: 'Mode', description: 'open (the default) or approved_only.', section: SECTION, default: 'open', enum: ['open', 'approved_only'] },
  ],
};

let writes: unknown[] = [];
let unapproved = 2;

beforeEach(() => {
  writes = [];
  unapproved = 2;
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/plugin_config_schemas/administration', () => HttpResponse.json({ sections: [CATALOGUE_SECTION] })),
    http.get('*/admin/plugin_config_values/administration/:section', () => HttpResponse.json({ values: { mode: 'open' } })),
    http.put('*/admin/plugin_config_values/administration/:section', async ({ request }) => {
      writes.push(await request.json());
      return HttpResponse.json({ saved: true, values: { mode: 'approved_only' } });
    }),
    http.get('*/admin/connector_inventory', () =>
      HttpResponse.json({ in_use: 3, without_approval: unapproved, connectors: [] }),
    ),
  );
});

afterEach(() => resetGeneratedClient());

async function chooseApprovedOnly(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.click(await screen.findByRole('combobox', { name: /Mode/ }));
  await user.click(within(screen.getByRole('listbox')).getByText('approved_only'));
}

describe('AdminConfiguration — catalogue mode guard', () => {
  it('warns with the count and asks before saving approved_only', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminConfiguration />);
    await chooseApprovedOnly(user);

    expect(await screen.findByTestId('catalogue-guard-warning')).toHaveTextContent('2 connector(s) in use have no approved request');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(writes).toHaveLength(0);
    await user.click(await screen.findByTestId('catalogue-guard-confirm'));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toEqual({ values: { mode: 'approved_only' } });
  });

  it('saves at once when every connector in use is approved', async () => {
    unapproved = 0;
    const user = userEvent.setup();
    renderAdminRoute(<AdminConfiguration />);
    await chooseApprovedOnly(user);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(screen.queryByTestId('catalogue-guard-warning')).not.toBeInTheDocument();
  });
});
