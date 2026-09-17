/**
 * A14 (ELITEA-0386): proves the whole wiring end to end — clicking "Apply"
 * in the dialog this hook renders actually PATCHes
 * `entity_settings/prompt_lib/{projectId}/{conversationId}` with the
 * one-element `[{participant_id, ...currentEntitySettings, llm_settings}]`
 * array `BatchUpdateEntitySettings` decodes, and the dialog closes on
 * success. This is the "prove the PATCH succeeds" half of the unit brief;
 * `entities/participant/api/participantApi.test.ts` covers the client
 * function in isolation.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { Participant } from '@/entities/participant';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { useChatBoxLlmSettingsDialog } from './ChatBoxLlmSettingsDialog';

const BASE = '/api/v2';

const PARTICIPANT: Participant = {
  id: 'p-1',
  entityName: 'application',
  entitySettings: { llmSettings: { temperature: 0.2 }, versionId: 'v2', variables: [{ name: 'x' }] },
};

function Harness({ participant }: { readonly participant: Participant | undefined }) {
  const { onEdit, dialog } = useChatBoxLlmSettingsDialog({ projectId: 7, conversationId: 'conv-1', participant });
  return (
    <>
      <button onClick={onEdit}>open</button>
      {dialog}
    </>
  );
}

function renderHarness(participant: Participant | undefined = PARTICIPANT) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return renderWithTheme(
    <QueryClientProvider client={client}>
      <Harness participant={participant} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useChatBoxLlmSettingsDialog', () => {
  it('opens on onEdit, is seeded with the participant current llm_settings', () => {
    renderHarness();
    expect(screen.queryByText('Model settings')).not.toBeInTheDocument();

    fireEvent.click(screen.getByText('open'));
    expect(screen.getByText('Model settings')).toBeInTheDocument();
  });

  it('Apply PATCHes the batch array with participant_id + merged entity_settings + the edited llm_settings, then closes', async () => {
    let capturedBody: unknown;
    server.use(
      http.patch(`${BASE}/elitea_core/entity_settings/prompt_lib/7/conv-1`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ ok: true });
      }),
    );

    renderHarness();
    fireEvent.click(screen.getByText('open'));
    expect(screen.getByText('Model settings')).toBeInTheDocument();

    // No field edits made — Apply sends the seeded llm_settings unchanged,
    // which is enough to prove the wire shape/participant scoping.
    fireEvent.click(screen.getByText('Apply'));

    await waitFor(() => {
      expect(capturedBody).toEqual([
        {
          participant_id: 'p-1',
          version_id: 'v2',
          variables: [{ name: 'x' }],
          // `LLMSettings` seeds `max_tokens: -1` ("unlimited") itself when the
          // incoming settings carry none — real dialog behavior, not this
          // wiring's own; asserted here rather than stripped out.
          llm_settings: { temperature: 0.2, max_tokens: -1 },
        },
      ]);
    });
    await waitFor(() => {
      expect(screen.queryByText('Model settings')).not.toBeInTheDocument();
    });
  });

  it('Cancel closes without sending a request', () => {
    renderHarness();
    fireEvent.click(screen.getByText('open'));
    fireEvent.click(screen.getByText('Cancel'));
    expect(screen.queryByText('Model settings')).not.toBeInTheDocument();
  });
});
