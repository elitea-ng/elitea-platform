import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { redirectToMarketplace } from './marketplaceRedirect';

interface Thrown {
  readonly options: {
    readonly to: string;
    readonly params?: Record<string, string>;
    readonly search?: Record<string, string>;
  };
}

async function redirectOf(
  search: { tab?: string; agentId?: string },
  project: string | undefined = '5',
): Promise<Thrown['options']> {
  const context = { auth: { getSelectedProjectId: () => project } };
  try {
    await redirectToMarketplace(search, context);
  } catch (thrown) {
    return (thrown as Thrown).options;
  }
  throw new Error('no redirect was thrown');
}

function mockSearch(rows: readonly { id: string }[]): void {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('/api/v2/elitea_core/marketplace_listings/prompt_lib/:projectId', ({ request }) => {
      const params = new URL(request.url).searchParams;
      return HttpResponse.json({
        rows: params.get('entity_id') === '42' && params.get('kind') === 'agent' ? rows : [],
        total: rows.length,
        facets: {},
      });
    }),
  );
}

describe('redirectToMarketplace', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('turns the shared ?agentId= link into the LISTING page found through the search route', async () => {
    mockSearch([{ id: '31' }]);
    expect(await redirectOf({ agentId: '42' })).toMatchObject({
      to: '/marketplace/$kind/$listingId',
      params: { kind: 'agents', listingId: '31' },
    });
  });

  it('reads agentId as an agent whatever the tab says', async () => {
    mockSearch([{ id: '31' }]);
    expect(await redirectOf({ tab: 'skills', agentId: '42' })).toMatchObject({
      params: { kind: 'agents', listingId: '31' },
    });
  });

  it('lands on the Marketplace home when the agent has no listing', async () => {
    mockSearch([]);
    expect(await redirectOf({ agentId: '42' })).toMatchObject({ to: '/marketplace' });
  });

  it('lands on the Marketplace home when the search fails or no project is selected', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.get('/api/v2/elitea_core/marketplace_listings/prompt_lib/:projectId', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    expect(await redirectOf({ agentId: '42' })).toMatchObject({ to: '/marketplace' });
    expect(await redirectOf({ agentId: '42' }, undefined)).toMatchObject({ to: '/marketplace' });
  });

  it('turns ?tab=skills into ?kind=skills', async () => {
    expect(await redirectOf({ tab: 'skills' })).toMatchObject({ to: '/marketplace', search: { kind: 'skills' } });
  });
});
