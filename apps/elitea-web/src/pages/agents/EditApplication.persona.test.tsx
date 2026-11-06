/**
 * ADR-0050 decision 1a — the Persona tab, exercised through the REAL agent
 * editor route: the tab is registered, its fields seed from the stored soul,
 * an edit arms Save, and the PUT body carries the soul Main validates.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { AgentSoulV1 } from '@/shared/api/generated/model';
import { getGetApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { server } from '@/test/setup';

import { EditApplication } from './EditApplication';
import { renderAgentsRoute } from './__tests__/testRouter';

installCodeMirrorTestPolyfills();

const STORED_SOUL: AgentSoulV1 = { schema: 'elitea.soul/v1', display_name: 'Ada', voice: ['dry', 'brief'] };

function detail(soul: AgentSoulV1 | null) {
  return {
    id: '42',
    name: 'My Agent',
    description: 'A helpful agent',
    icon: '',
    owner_id: 'user-1',
    created_at: '2026-01-01T00:00:00Z',
    versions: [{ id: '1', name: 'base', status: 'draft', agent_type: 'classic', created_at: '2026-01-01T00:00:00Z' }],
    version_details: {
      id: '1',
      application_id: '42',
      name: 'base',
      status: 'draft',
      instructions: 'Be helpful.',
      conversation_starters: [],
      soul,
    },
  };
}

function captureVersionSave(sink: Record<string, unknown>[]): void {
  server.use(
    http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
      sink.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json({ id: '1', application_id: '42', name: 'base', status: 'draft' }, { status: 201 });
    }),
    http.put('*/elitea_core/application/prompt_lib/:projectId/:id', () => HttpResponse.json({ id: '42' }, { status: 201 })),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/configurations/models/:projectId', () =>
      HttpResponse.json({
        items: [{ name: 'gpt-4o', display_name: 'GPT-4o', project_id: '9', default: true }],
        default_model_name: 'gpt-4o',
      }),
    ),
    http.get('*/social/author*', () => HttpResponse.json({ id: '6', name: 'Driver', avatar: '' })),
  );
});

afterEach(() => resetGeneratedClient());

describe('EditApplication — the Persona tab', () => {
  it('seeds from the stored soul and saves an edit as the soul object', async () => {
    server.use(getGetApplicationMockHandler(detail(STORED_SOUL)));
    const bodies: Record<string, unknown>[] = [];
    captureVersionSave(bodies);
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
    const user = userEvent.setup();

    await user.click(await screen.findByTestId('edit-application-tab-persona', {}, { timeout: 5_000 }));
    const name = await screen.findByTestId('persona-displayName');
    await waitFor(() => expect(name).toHaveValue('Ada'));
    expect(screen.getByTestId('persona-voice')).toHaveValue('dry\nbrief');

    await user.type(screen.getByTestId('persona-role'), 'Release reviewer');
    await waitFor(() => expect(screen.getByTestId('agent-save-button')).toBeEnabled());
    await user.click(screen.getByTestId('agent-save-button'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]?.['soul']).toEqual({ ...STORED_SOUL, role: 'Release reviewer' });
  }, 20_000);

  it('forwards an untouched stored soul unchanged on an unrelated save', async () => {
    server.use(getGetApplicationMockHandler(detail({ ...STORED_SOUL, body: '  keep my spacing  ' })));
    const bodies: Record<string, unknown>[] = [];
    captureVersionSave(bodies);
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
    const user = userEvent.setup();

    await waitFor(() => expect(screen.getByTestId('agent-name-input')).toHaveValue('My Agent'), { timeout: 5_000 });
    await user.type(screen.getByTestId('agent-description-input'), '!');
    await waitFor(() => expect(screen.getByTestId('agent-save-button')).toBeEnabled());
    await user.click(screen.getByTestId('agent-save-button'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]?.['soul']).toEqual({ ...STORED_SOUL, body: '  keep my spacing  ' });
  }, 20_000);
});
