/**
 * Composition root for Settings › Project Context (issue 841).
 *
 * The page is `@ts-nocheck`, so the type checker says nothing about it, and
 * it had no test of its own. These cover the parity gaps this change closes,
 * each measured against the reference:
 *
 *  - the enable toggle SAVES (`ProjectContextSavedView.jsx:30-40`); it only
 *    set local state here, so switching the context off and leaving the tab
 *    left it on;
 *  - an over-long markdown import is REPORTED
 *    (`ProjectContextEditor.jsx:127-146`); it wrote to the console here, so
 *    the import silently did nothing;
 *  - the AI control is called "Build with AI" on both screens
 *    (`GenerateProjectContextButton.jsx:14`); the editor toolbar called it
 *    "Generate with AI";
 *  - the editor can copy the context out (the reference has it in the
 *    toolbar and again in the saved view's menu); this port had neither.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { configure, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetBackendCapabilitiesForTests, setBackendCapabilityForTests } from '@/shared/config/backendCapabilities';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '@/test/setup';

import { ProjectContext } from './ProjectContext';

configure({ asyncUtilTimeout: 5_000 });
vi.setConfig({ testTimeout: 30_000 });

const BASE = '/api/v2';
const CONTEXT_PATH = `${BASE}/elitea_core/project_context/prompt_lib/:projectId/project-context`;
const PERMISSIONS_PATH = `${BASE}/auth/permissions/prompt_lib/:projectId`;

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithTheme(
    <QueryClientProvider client={client}>
      <ProjectContext
        projectId="7"
        projectName="Demo"
        canView
        canEdit
      />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  // `GenerateProjectContextButton` reads the permission set itself; the
  // page's own `canEdit` prop does not reach it.
  server.use(
    http.get(PERMISSIONS_PATH, () =>
      HttpResponse.json([
        { name: 'models.project_context.view', enabled: true },
        { name: 'models.project_context.edit', enabled: true },
      ]),
    ),
  );
  // The AI draft control hides itself on a deployment that does not serve
  // `generate_project_context_draft`; the reference gates it on permission
  // only. This build serves it, so the toolbar shows the control.
  setBackendCapabilityForTests('aiGeneration', true);
});

afterEach(() => {
  resetGeneratedClient();
  resetBackendCapabilitiesForTests();
});

describe('Settings › Project Context', () => {
  it('saves the enable toggle immediately, sending the SAVED content', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.get(CONTEXT_PATH, () => HttpResponse.json({ content: 'Stored background', enabled: true })),
      http.put(CONTEXT_PATH, async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ content: 'Stored background', enabled: false });
      }),
    );

    mount();
    const user = userEvent.setup();

    const toggle = await screen.findByRole('switch');
    await user.click(toggle);

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ content: 'Stored background', enabled: false });
  });

  it('puts the toggle back and reports the failure when the save is refused', async () => {
    server.use(
      http.get(CONTEXT_PATH, () => HttpResponse.json({ content: 'Stored background', enabled: true })),
      http.put(CONTEXT_PATH, () => HttpResponse.json({ error: 'nope' }, { status: 500 })),
    );

    mount();
    const user = userEvent.setup();

    const toggle = await screen.findByRole('switch');
    await user.click(toggle);

    expect(await screen.findByText('Failed to save Project Context')).toBeInTheDocument();
    await waitFor(() => expect(toggle).toBeChecked());
  });

  it('names the AI control "Build with AI" in the editor toolbar and offers a copy control', async () => {
    server.use(http.get(CONTEXT_PATH, () => HttpResponse.json({ content: 'Stored background', enabled: true })));

    mount();

    expect(await screen.findByTestId('project-context-generate-with-ai-button')).toBeInTheDocument();
    // One name for one control. The empty state already said "Build with
    // AI"; the editor toolbar said "Generate with AI".
    expect(screen.getByText('Build with AI')).toBeInTheDocument();
    expect(screen.queryByText('Generate with AI')).toBeNull();
    expect(screen.getByTestId('project-context-copy-button')).toBeEnabled();
    expect(screen.getByTestId('project-context-save-button')).toBeInTheDocument();
  });

  it('shows the empty state, and its own Build with AI control, when nothing is saved', async () => {
    server.use(http.get(CONTEXT_PATH, () => HttpResponse.json({ content: '', enabled: true })));

    mount();

    expect(await screen.findByTestId('project-context-empty-state')).toBeInTheDocument();
    expect(screen.getByTestId('project-context-build-with-ai-button')).toBeInTheDocument();
  });
});
