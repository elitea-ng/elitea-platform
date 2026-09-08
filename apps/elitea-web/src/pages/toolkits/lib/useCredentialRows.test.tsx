/*
 * `useCredentialRows` — the rows a credential picker offers.
 *
 * It had no test, and it is the worst kind of thing to leave untested: every
 * one of its branches decides whether a saved credential APPEARS, and a row
 * that does not appear looks exactly like a credential the user never created.
 *
 * The four branches, and what each one costs when it is wrong:
 *
 *  - the PERSONAL query is skipped for public-only pickers, for a viewer with
 *    no personal project, when the personal project IS the selected one, and
 *    for the `vectorstorage` section. Skipping one it should run loses the
 *    user's own credentials; running one it should skip lists a duplicate of
 *    every row under the selected project.
 *  - `configuration_types` filters. An empty list accepts every type — a
 *    filter applied to an empty list would offer nothing at all.
 *  - a row with no title is DROPPED, because the value codec encodes the empty
 *    string as "no selection" and picking such a row silently clears the field.
 *  - `hasFetchedData` must stay false while either query is pending, or a saved
 *    value renders as "not found" for a frame.
 *
 * Driven through MSW against the real configurations endpoint, so the page
 * shape the hook normalises is the one the server sends.
 */
import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { createTestQueryClient } from '@/features/toolkits/__tests__/testUtils';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useCredentialRows } from './useCredentialRows';
import type { UseCredentialRowsParams, UseCredentialRowsResult } from './useCredentialRows';

interface WireRow {
  readonly id?: number;
  readonly type: string;
  readonly elitea_title?: string;
  readonly label?: string;
  readonly project_id?: string;
  readonly data?: Record<string, unknown>;
}

interface Page {
  readonly items: readonly WireRow[];
  readonly total: number;
  readonly limit: number;
  readonly offset: number;
  readonly shared?: { readonly items: readonly WireRow[]; readonly total: number };
}

const EMPTY: Page = { items: [], total: 0, limit: 500, offset: 0 };

/** The requests the hook issued, keyed by the project id in the path. */
let requestedProjects: string[] = [];

function servePages(pages: Readonly<Record<string, Page>>): void {
  server.use(
    http.get('/api/v2/configurations/configurations/:projectId', ({ params }) => {
      const projectId = String(params['projectId']);
      requestedProjects.push(projectId);
      return HttpResponse.json(pages[projectId] ?? EMPTY);
    }),
  );
}

beforeEach(() => {
  requestedProjects = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

function renderCredentialRows(
  params: UseCredentialRowsParams,
  personalProjectId: string | undefined,
): { current: UseCredentialRowsResult | undefined } {
  const box: { current: UseCredentialRowsResult | undefined } = { current: undefined };

  function Probe() {
    box.current = useCredentialRows(params);
    return null;
  }

  const rootRoute = createRootRoute({ component: Probe });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: {
      auth: {
        getUser: () =>
          personalProjectId === undefined ? {} : { personal_project_id: personalProjectId },
      },
    },
  });

  render(
    <QueryClientProvider client={createTestQueryClient()}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return box;
}

function params(overrides: Partial<UseCredentialRowsParams> = {}): UseCredentialRowsParams {
  return {
    projectId: '1',
    section: 'credentials',
    configurationTypes: [],
    onlyPublic: false,
    ...overrides,
  };
}

describe('useCredentialRows', () => {
  it('lists the project rows, its shared rows and the viewer\'s personal rows', async () => {
    servePages({
      '1': {
        ...EMPTY,
        items: [{ id: 1, type: 'github', elitea_title: 'Project token', project_id: '1' }],
        shared: {
          items: [{ id: 2, type: 'jira', elitea_title: 'Shared jira', project_id: '9' }],
          total: 1,
        },
      },
      '42': {
        ...EMPTY,
        items: [{ id: 3, type: 'github', elitea_title: 'My token', project_id: '42' }],
      },
    });

    const state = renderCredentialRows(params(), '42');
    await waitFor(() => expect(state.current?.hasFetchedData).toBe(true));

    expect(state.current?.rows.map((row) => row.eliteaTitle)).toEqual([
      'Project token',
      'Shared jira',
      'My token',
    ]);
    // Only the personal half is marked private — that flag is what the toolkit
    // settings persist beside the title.
    expect(state.current?.rows.map((row) => row.isPrivate)).toEqual([false, false, true]);
    // The row carries the project it BELONGS to, not the selected one.
    expect(state.current?.rows.map((row) => row.ownerProjectId)).toEqual(['1', '9', '42']);
  });

  it('skips the personal query for a public-only picker', async () => {
    servePages({ '1': EMPTY, '42': EMPTY });
    const state = renderCredentialRows(params({ onlyPublic: true }), '42');
    await waitFor(() => expect(state.current?.hasFetchedData).toBe(true));
    expect(requestedProjects).toEqual(['1']);
  });

  it('skips the personal query for the vectorstorage section', async () => {
    servePages({ '1': EMPTY, '42': EMPTY });
    const state = renderCredentialRows(params({ section: 'vectorstorage' }), '42');
    await waitFor(() => expect(state.current?.hasFetchedData).toBe(true));
    expect(requestedProjects).toEqual(['1']);
  });

  it('skips the personal query when the personal project IS the selected one', async () => {
    // Otherwise every row would be listed twice, once as shared and once as
    // private — the duplicate the baseline's own branch exists to avoid.
    servePages({ '42': { ...EMPTY, items: [{ id: 1, type: 'github', elitea_title: 'Mine' }] } });
    const state = renderCredentialRows(params({ projectId: '42' }), '42');
    await waitFor(() => expect(state.current?.hasFetchedData).toBe(true));
    expect(requestedProjects).toEqual(['42']);
    expect(state.current?.rows).toHaveLength(1);
  });

  it('skips the personal query for a viewer with no personal project', async () => {
    servePages({ '1': EMPTY });
    const state = renderCredentialRows(params(), undefined);
    await waitFor(() => expect(state.current?.hasFetchedData).toBe(true));
    expect(requestedProjects).toEqual(['1']);
  });

  it('keeps only the accepted configuration types, and accepts every type when none is named', async () => {
    servePages({
      '1': {
        ...EMPTY,
        items: [
          { id: 1, type: 'github', elitea_title: 'gh' },
          { id: 2, type: 'jira', elitea_title: 'jira' },
        ],
      },
    });
    const filtered = renderCredentialRows(
      params({ configurationTypes: ['jira'] }),
      undefined,
    );
    await waitFor(() => expect(filtered.current?.hasFetchedData).toBe(true));
    expect(filtered.current?.rows.map((row) => row.type)).toEqual(['jira']);
  });

  it('drops a row with no usable title rather than offering one that clears the field', async () => {
    servePages({
      '1': {
        ...EMPTY,
        items: [
          { id: 1, type: 'github' },
          { id: 2, type: 'github', elitea_title: '   ' },
          // A title carried in `data` instead of at the top level. The
          // baseline reads both, in this order.
          { id: 3, type: 'github', data: { title: 'from data' } },
        ],
      },
    });
    const state = renderCredentialRows(params(), undefined);
    await waitFor(() => expect(state.current?.hasFetchedData).toBe(true));
    expect(state.current?.rows.map((row) => row.eliteaTitle)).toEqual(['from data']);
  });

  it('prefers the row\'s own label for the displayed option', async () => {
    servePages({
      '1': {
        ...EMPTY,
        items: [
          { id: 1, type: 'github', elitea_title: 'title', label: 'Displayed' },
          { id: 2, type: 'github', elitea_title: 'title-2', label: '  ' },
        ],
      },
    });
    const state = renderCredentialRows(params(), undefined);
    await waitFor(() => expect(state.current?.hasFetchedData).toBe(true));
    // A blank label falls back to the title; the value the form stores stays
    // the TITLE either way.
    expect(state.current?.rows.map((row) => row.displayLabel)).toEqual(['Displayed', 'title-2']);
    expect(state.current?.rows.map((row) => row.eliteaTitle)).toEqual(['title', 'title-2']);
  });

  /*
   * The address a row names, which is what decides whether the option carries
   * its "open in a new tab" action.
   *
   * `CredentialOptionLabel` renders that control on a truthy `credentialUrl`
   * and on nothing else. The hook never set the field, so the control was
   * absent from every credential picker in the application while its selector,
   * its prop and its own unit test all went on passing.
   */
  it('carries the provider address a row names, and none where the row names one', async () => {
    servePages({
      '1': {
        ...EMPTY,
        items: [
          { id: 1, type: 'github', elitea_title: 'has-base-url', data: { base_url: 'https://github.example/api' } },
          { id: 2, type: 'jira', elitea_title: 'has-url', data: { url: 'https://jira.example' } },
          { id: 3, type: 'github', elitea_title: 'has-neither', data: { access_token: '{{secret.x}}' } },
        ],
      },
    });

    const state = renderCredentialRows(params({ onlyPublic: true }), undefined);
    await waitFor(() => expect(state.current?.hasFetchedData).toBe(true));

    expect(state.current?.rows.map((row) => row.credentialUrl)).toEqual([
      'https://github.example/api',
      'https://jira.example',
      undefined,
    ]);
  });

  it('reports no data at all, and issues no request, without a project', async () => {
    servePages({});
    const state = renderCredentialRows(params({ projectId: undefined }), '42');
    await waitFor(() => expect(state.current).toBeDefined());
    expect(state.current?.hasFetchedData).toBe(false);
    expect(state.current?.rows).toEqual([]);
    expect(requestedProjects).not.toContain('');
  });

  it('refreshes both halves, and only the project half when the personal one is skipped', async () => {
    servePages({ '1': EMPTY, '42': EMPTY });
    const both = renderCredentialRows(params(), '42');
    await waitFor(() => expect(both.current?.hasFetchedData).toBe(true));
    requestedProjects = [];
    both.current?.refresh();
    await waitFor(() => expect(requestedProjects).toEqual(expect.arrayContaining(['1', '42'])));

    const projectOnly = renderCredentialRows(params({ onlyPublic: true }), '42');
    await waitFor(() => expect(projectOnly.current?.hasFetchedData).toBe(true));
    requestedProjects = [];
    projectOnly.current?.refresh();
    await waitFor(() => expect(requestedProjects).toContain('1'));
    expect(requestedProjects).not.toContain('42');
  });
});
