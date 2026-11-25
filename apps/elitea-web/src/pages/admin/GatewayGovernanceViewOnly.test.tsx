/**
 * Admin › LLM Governance in read-only mode (R3.4b, issue 2613): at the read
 * level of `llm-governance` the notice shows, New entry, the guardrail preset
 * and every row action are disabled rather than hidden, and search still
 * filters the list.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminGatewayGovernance } from './GatewayGovernance';
import { renderAdminRoute } from './__tests__/testRouter';
import { enabledWriteControls, findViewOnlyNotice, serveConsoleLevel } from './__tests__/viewOnly';

const ROWS = [
  { id: 'g1', type: 'budget', section: 'governance', name: 'team-budget', data: {}, enabled: true },
  { id: 'g2', type: 'rate_limit', section: 'governance', name: 'burst-cap', data: {}, enabled: true },
];

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(http.get('*/admin/gateway/governance', () => HttpResponse.json({ rows: ROWS, total: ROWS.length })));
});

afterEach(() => {
  resetGeneratedClient();
});

describe('Admin › LLM Governance › view only', () => {
  it('disables every write at the read level and keeps search working', async () => {
    serveConsoleLevel('read', 'llm-governance');
    renderAdminRoute(<AdminGatewayGovernance />);

    await screen.findByText('team-budget');
    expect(await findViewOnlyNotice()).toHaveAttribute('data-item-id', 'llm-governance');
    await waitFor(() => expect(screen.getByTestId('governance-create')).toBeDisabled());
    expect(screen.getByTestId('governance-guardrail-preset')).toBeDisabled();
    for (const button of screen.getAllByRole('button', { name: /Edit entry|Delete entry/ })) {
      expect(button).toBeDisabled();
    }
    expect(enabledWriteControls()).toEqual([]);

    await userEvent.setup().type(screen.getByRole('textbox'), 'burst');
    await waitFor(() => expect(screen.queryByText('team-budget')).not.toBeInTheDocument());
    expect(screen.getByText('burst-cap')).toBeInTheDocument();
  });

  it('keeps the writes enabled at the write level', async () => {
    serveConsoleLevel('write', 'llm-governance');
    renderAdminRoute(<AdminGatewayGovernance />);

    await screen.findByText('team-budget');
    await waitFor(() => expect(screen.getByTestId('governance-create')).toBeEnabled());
    expect(screen.queryByTestId('admin-view-only')).not.toBeInTheDocument();
  });
});
