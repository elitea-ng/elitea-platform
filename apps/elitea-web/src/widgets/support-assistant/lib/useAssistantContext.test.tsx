/**
 * The page-context derivation — the thing that makes an IN-APP assistant
 * different from a search box.
 *
 * The entity-id rule is the part worth pinning: this app's entity routes
 * interpose a TAB (`/agents/configuration/42`), so "the segment after the kind"
 * is the tab name on every one of them, and a derivation that took it would
 * report `current_entity_id` for a string like `configuration` or nothing at
 * all.
 */
import type { ReactNode } from 'react';

import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { deriveAssistantPageContext, useAssistantContext } from './useAssistantContext';

function wrapperAt(pathname: string) {
  return function Wrapper({ children }: { readonly children: ReactNode }): ReactNode {
    const rootRoute = createRootRoute({ component: () => <>{children}</> });
    const router = createRouter({
      routeTree: rootRoute,
      history: createMemoryHistory({ initialEntries: [pathname] }),
    });
    return <RouterProvider router={router as never} />;
  };
}

describe('deriveAssistantPageContext', () => {
  it('reports the entity from the LAST numeric segment, not the one after the kind', () => {
    expect(deriveAssistantPageContext('/agents/configuration/42')).toEqual({
      current_page: '/agents/configuration/42',
      current_entity_type: 'agent',
      current_entity_id: 42,
    });
  });

  it('handles a versioned entity route', () => {
    const context = deriveAssistantPageContext('/agents/configuration/42/7');
    expect(context.current_entity_type).toBe('agent');
    // The version is the last numeric segment on this route. It is reported as
    // the entity id, which is WRONG-ish and deliberate: the alternative is a
    // per-route table this widget would have to keep in step with the router.
    // The agent is still named by `current_page`, which carries the whole path.
    expect(context.current_entity_id).toBe(7);
  });

  it('reports NO entity on a listing', () => {
    const context = deriveAssistantPageContext('/agents');
    expect(context.current_page).toBe('/agents');
    expect(context.current_entity_type).toBe('agent');
    expect('current_entity_id' in context).toBe(false);
  });

  it('reports NO entity type for a page that names no entity kind', () => {
    const context = deriveAssistantPageContext('/settings/profile');
    expect(context.current_page).toBe('/settings/profile');
    expect('current_entity_type' in context).toBe(false);
    expect('current_entity_id' in context).toBe(false);
  });

  it('omits absent fields as KEYS rather than as undefined values', () => {
    // The payload is JSON-serialised and sent to an agent. `"current_entity_id":
    // null` reads as "there is an entity and it has no id"; an absent key reads
    // as "not on an entity page", which is the truth.
    expect(JSON.stringify(deriveAssistantPageContext('/help-center'))).toBe(
      JSON.stringify({ current_page: '/help-center' }),
    );
  });

  it('maps every entity route segment this app actually has', () => {
    for (const [path, expected] of [
      ['/pipelines/configuration/1', 'pipeline'],
      ['/skills/configuration/1', 'skill'],
      ['/toolkits/configuration/1', 'toolkit'],
      ['/mcps/1', 'mcp'],
      ['/artifacts/1', 'artifact'],
      ['/credentials/1', 'credential'],
      ['/chat/1', 'conversation'],
    ] as const) {
      expect(deriveAssistantPageContext(path).current_entity_type).toBe(expected);
    }
  });

  it('survives the root path', () => {
    expect(deriveAssistantPageContext('/')).toEqual({ current_page: '/' });
  });
});

describe('useAssistantContext', () => {
  it('adds the project id and name when the shell passed a project', async () => {
    const { result } = renderHook(() => useAssistantContext({ id: 7, name: 'Acme' }), {
      wrapper: wrapperAt('/agents'),
    });

    await waitFor(() => {
      expect(result.current).toEqual(
        expect.objectContaining({ current_page: '/agents', project_id: 7, project_name: 'Acme' }),
      );
    });
  });

  it('omits the project id when it does not resolve to a finite number', async () => {
    const { result } = renderHook(() => useAssistantContext({ id: 'not-a-number' }), {
      wrapper: wrapperAt('/agents'),
    });

    await waitFor(() => {
      expect(result.current).not.toBeNull();
    });
    expect('project_id' in result.current).toBe(false);
  });

  it('omits the project name when it is an empty string', async () => {
    const { result } = renderHook(() => useAssistantContext({ id: 1, name: '' }), {
      wrapper: wrapperAt('/agents'),
    });

    await waitFor(() => {
      expect(result.current).not.toBeNull();
    });
    expect('project_name' in result.current).toBe(false);
  });

  it('builds a context with no project fields when none is passed', async () => {
    const { result } = renderHook(() => useAssistantContext(), { wrapper: wrapperAt('/agents') });

    await waitFor(() => {
      expect(result.current).toEqual({ current_page: '/agents', current_entity_type: 'agent' });
    });
  });
});
