/**
 * useWikiList: what it loads, and what it refuses to show.
 *
 * THE DATA IS THE RECORDED PROVIDER'S. The bucket listing is listObjects'
 * example in v2.yaml (served by the generated `getListObjectsResponseMock`),
 * and MANIFEST below is the schema-2 manifest that listing names. Both carry
 * keys produced by EXECUTING the legacy plugin (phase-P0 conformance
 * recording), so this hook is not tested against a store that does not exist.
 *
 * THE SECOND TEST IS THE ONE THAT MATTERS. A bucket holds every wiki any
 * project generated, so a hook that returned everything it listed would show a
 * project its neighbour's wikis. The repository filter is what prevents that,
 * and this asserts it runs — not that it exists.
 */
import { waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { WikiManifest } from '@/entities/wiki';
import { getListObjectsResponseMock } from '@/shared/api/generated/artifacts/artifacts.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';
import { useWikiList } from './useWikiList';

const PROJECT = '1';
const BUCKET = 'wiki-artifacts';
const OBJECTS = `/api/v2/artifacts/objects/${PROJECT}/${BUCKET}`;

/** The identity the recorded manifest belongs to. */
const OWN_IDENTITY = { repository: 'acme/notes-service', branch: 'main' };

/** The manifest object the example listing holds (a stored file, not an API body). */
const MANIFEST: WikiManifest = {
  schema_version: 2,
  wiki_id: 'acme--notes-service--main',
  wiki_title: 'notes-service',
  description: 'A small notes service with SQLite persistence. Store, search and authorise access to notes.',
  wiki_version_id: '20260101T000000Z-0000000a',
  created_at: '2026-01-01T00:00:00+00:00',
  canonical_repo_identifier: 'acme/notes-service:main:0000000',
  repository: 'acme/notes-service',
  branch: 'main',
  analysis_key: 'acme/notes-service:main:0000000@20260101T000000Z-0000000a',
  pages: [
    'acme--notes-service--main/wiki_pages/overview/getting-started.md',
    'acme--notes-service--main/wiki_pages/overview/architecture-data-flow.md',
    'acme--notes-service--main/wiki_pages/components/note-storage.md',
    'acme--notes-service--main/wiki_pages/components/search-ranking.md',
    'acme--notes-service--main/wiki_pages/components/bearer-tokens.md',
  ],
  provider_type: 'github',
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

function serveBucket(manifestBody: object = MANIFEST): void {
  server.use(
    http.get(OBJECTS, () => HttpResponse.json(getListObjectsResponseMock())),
    http.get(`${OBJECTS}/*`, () => HttpResponse.json(manifestBody)),
  );
}

describe('the listing and the manifest agree', () => {
  // The wiki browser lists objects and then reads the pages the manifest
  // names. A listing that omitted a named page would let the browser tests
  // pass against an inconsistency the real store never produces (the first
  // draft of this data listed two of the manifest's five pages).
  it('every page the manifest names is in the listing', () => {
    const listed = new Set(getListObjectsResponseMock().objects.map((o) => o.key));
    expect((MANIFEST.pages ?? []).filter((page) => !listed.has(page))).toEqual([]);
  });

  it('no key speaks the legacy artifact path family', () => {
    // elitea-main serves no /api/v2/artifacts/artifact(s)/default/... route.
    expect(JSON.stringify([getListObjectsResponseMock(), MANIFEST])).not.toMatch(/artifacts\/artifacts?\/default/);
  });
});

describe('useWikiList', () => {
  it('loads the manifests the bucket lists', async () => {
    serveBucket();
    const { result } = renderHookWithProviders(() => useWikiList(PROJECT, OWN_IDENTITY));

    await waitFor(() => {
      expect(result.current.isSuccess).toBe(true);
    });
    expect(result.current.data?.allWikis).toHaveLength(1);
    expect(result.current.data?.allWikis[0]?.wiki_id).toBe('acme--notes-service--main');
  });

  it('does not return a wiki belonging to a different repository', async () => {
    serveBucket();
    const { result } = renderHookWithProviders(() =>
      useWikiList(PROJECT, { repository: 'other-org/other-service', branch: 'main' }),
    );

    await waitFor(() => {
      expect(result.current.isSuccess).toBe(true);
    });
    // The bucket listed one wiki and it is not this project's.
    expect(result.current.data?.allWikis).toHaveLength(1);
    expect(result.current.data?.wikis).toEqual([]);
  });

  it('returns nothing at all when no repository is configured', async () => {
    serveBucket();
    const { result } = renderHookWithProviders(() => useWikiList(PROJECT, null));

    await waitFor(() => {
      expect(result.current.isSuccess).toBe(true);
    });
    expect(result.current.data?.wikis).toEqual([]);
  });

  it('does not fetch while disabled', async () => {
    let requests = 0;
    server.use(
      http.get(OBJECTS, () => {
        requests += 1;
        return HttpResponse.json(getListObjectsResponseMock());
      }),
    );
    const { result } = renderHookWithProviders(() =>
      useWikiList(PROJECT, OWN_IDENTITY, { enabled: false }),
    );

    // The capability flag is off by default, so this is the state the route
    // actually renders in today. A hook that fetched anyway would put requests
    // on the wire for a feature the build says it does not serve.
    await waitFor(() => {
      expect(result.current.fetchStatus).toBe('idle');
    });
    expect(requests).toBe(0);
  });
});
