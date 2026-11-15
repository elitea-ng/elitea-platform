import { createMemoryHistory, createRouter, RouterProvider } from '@tanstack/react-router';
import { cleanup, render, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { AuthContext, RouterContext } from '@/app/router-context';
import { resetConfigForTests } from '@/shared/config/get-config';

import { routeTree } from '../../routeTree.gen';

/**
 * Route-tree smoke coverage (spec §9.3 R1, 90% floor). Mounts the REAL
 * generated route tree through a REAL `RouterProvider` (§6.2: no router
 * mocking) at a concrete URL for every one of the 71 §8.1 patterns this
 * unit owns (all except ROUTE-070/071, unit R3's), asserting each settles
 * to `idle` without throwing. This is what actually exercises every leaf
 * route file's `component`/`beforeLoad`/`validateSearch` — the per-file
 * unit tests above prove BEHAVIOUR for the interesting cases (guards,
 * search-param rejection, the settings anomaly); this proves every
 * remaining minimal shell (task item 6) mounts cleanly end to end.
 *
 * A permissive auth context is used throughout (all permissions granted,
 * non-public project) so the P8 permission guards and SkillsGuard let
 * these paths render instead of redirecting — guard REDIRECT behaviour
 * itself is covered by `guards.test.ts`/`guardsIntegration.test.tsx`.
 */

const permissiveAuth: AuthContext = {
  getUser: () => ({
    id: 'u1',
    personal_project_id: 'p1',
    permissions: ['models.chat.folders.get', 'configuration.artifacts.artifacts.view'],
    publicPermissions: [],
  }),
  getSelectedProjectId: () => '999', // not the public project (VITE_PUBLIC_PROJECT_ID='11')
};

const PATHS: readonly string[] = [
  '/',
  '/onboarding',
  '/help-center',
  '/agents-hub',
  '/elitea-catalog',
  '/marketplace',
  '/marketplace?kind=skills',
  '/marketplace/agents/1',
  '/marketplace/skills/1',
  '/mode-switch',
  '/mcp-auth-callback',
  '/chat',
  '/chat/conv-1',
  '/agents',
  '/agents/create',
  '/agents/latest',
  '/agents/latest/agent-1',
  '/agents/latest/agent-1/v1',
  '/skills',
  '/skills/create',
  '/skills/all',
  '/skills/all/skill-1',
  '/skills/all/skill-1/v1',
  '/pipelines',
  '/pipelines/create',
  '/pipelines/latest',
  '/pipelines/latest/pipe-1',
  '/pipelines/latest/pipe-1/v1',
  '/credentials',
  '/credentials/all',
  '/credentials/all/cred-1',
  '/credentials/create-credential',
  '/credentials/create-credential/oauth',
  '/toolkits',
  '/toolkits/create',
  '/toolkits/create/github',
  '/toolkits/all',
  '/toolkits/all/tk-1',
  '/mcps',
  '/mcps/create',
  '/mcps/create/sse',
  '/mcps/all',
  '/mcps/all/mcp-1',
  '/apps',
  '/apps/create',
  '/apps/create/agent',
  '/apps/applications',
  '/apps/applications/app-1',
  '/user-public/agents',
  '/user-public/agents/agent-1',
  '/user-public/agents/agent-1/v1',
  '/user-public/pipelines/pipe-1',
  '/user-public/pipelines/pipe-1/v1',
  '/user-public/toolkits/tk-1',
  '/user-public/mcps/mcp-1',
  '/user-public/apps/app-1',
  '/artifacts',
  '/artifacts/create-bucket',
  '/settings',
  '/settings/model-configuration',
  '/settings/environment',
  '/settings/project-params',
  '/settings/prompts',
  '/settings/tokens',
  '/settings/secrets',
  '/settings/users',
  '/settings/analytics',
  '/settings/personalization',
  '/settings/notifications',
  '/settings/create-configuration',
  '/settings/create-configuration/oauth',
  '/settings/edit-configuration/cred-uid-1',
  '/settings/create-personal-token',
];

beforeEach(() => {
  resetConfigForTests();
  vi.stubEnv('VITE_SERVER_URL', 'https://elitea.example');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', '11');
});

afterEach(() => {
  cleanup();
  vi.unstubAllEnvs();
  resetConfigForTests();
});

describe('all owned route patterns mount without crashing', () => {
  for (const path of PATHS) {
    it(`mounts ${path}`, async () => {
      const history = createMemoryHistory({ initialEntries: [path] });
      const router = createRouter({
        routeTree,
        history,
        context: { auth: permissiveAuth } satisfies RouterContext,
      });
      render(<RouterProvider router={router} />);

      await waitFor(() => {
        expect(router.state.status).toBe('idle');
      });
      // A route-level error (thrown in a component/loader) surfaces as
      // `router.state.matches` containing an error match rather than a
      // thrown exception from `render()` in some configurations — assert
      // directly that nothing in the resolved match list is errored.
      type RM = { status?: string; error?: unknown };
      const erroredMatch = (router.state.matches as RM[]).find((match) => match.status === 'error');
      expect(erroredMatch, `route at ${path} errored: ${JSON.stringify(erroredMatch?.error)}`).toBeUndefined();
    });
  }
});
