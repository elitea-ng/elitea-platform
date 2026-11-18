/**
 * The agent page has two model pickers: the form's (`AgentModelSettings`) and
 * the test chat's composer. They must be one setting (#2151). The harness
 * holds the version's `llm_settings` the way the page does and mounts the
 * form's picker beside a stand-in for the chat's, driven only by what
 * `useAgentChatModelBinding` hands `ChatBox` as its `llm` prop.
 */
import { useState } from 'react';
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { AgentModelSettings } from '../ui/AgentModelSettings';

import { useAgentChatModelBinding } from './useAgentChatModelBinding';

const BASE = '/api/v2';
const PROJECT_ID = '17';

const CATALOGUE = {
  items: [
    { name: 'gpt-4o', display_name: 'GPT-4o', project_id: 17, default: true },
    { name: 'o3-mini', display_name: 'O3 Mini', project_id: 17, supports_reasoning: true },
  ],
  default_model_name: 'gpt-4o',
};

/** A stand-in for the chat composer's picker: it shows `llm.settings` and answers a pick through `llm.onSetSettings`. */
function ChatPickerStandIn({ value, onChange }: { value: AgentLlmSettings | undefined; onChange: (next: AgentLlmSettings) => void }): ReactNode {
  const llm = useAgentChatModelBinding(PROJECT_ID, value, onChange, false);
  return (
    <div>
      <output data-testid="chat-model">{typeof llm?.settings?.['model_name'] === 'string' ? llm.settings['model_name'] : 'default'}</output>
      <button type="button" onClick={() => llm?.onSetSettings({ model_name: 'o3-mini', model_project_id: 17 })}>
        pick in chat
      </button>
    </div>
  );
}

function Page(): ReactNode {
  const [value, setValue] = useState<AgentLlmSettings | undefined>(undefined);
  return (
    <>
      <AgentModelSettings projectId={PROJECT_ID} value={value} onChange={setValue} />
      <ChatPickerStandIn value={value} onChange={setValue} />
      <output data-testid="form-value">{JSON.stringify(value ?? null)}</output>
    </>
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(http.get(`${BASE}/configurations/models/${PROJECT_ID}`, () => HttpResponse.json(CATALOGUE)));
});

afterEach(() => {
  resetGeneratedClient();
});

function renderPage(): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={queryClient}>
      <Page />
    </QueryClientProvider>,
  );
}

describe('useAgentChatModelBinding', () => {
  it('shows the model picked in the form in the chat at once', async () => {
    renderPage();
    expect(screen.getByTestId('chat-model')).toHaveTextContent('default');

    const user = userEvent.setup();
    await user.click(await screen.findByTestId('model-selector-name'));
    await user.click(await screen.findByRole('option', { name: /O3 Mini/ }));

    await waitFor(() => expect(screen.getByTestId('chat-model')).toHaveTextContent('o3-mini'));
  });

  it('writes a model picked in the chat into the form, as a complete profile with a numeric project id', async () => {
    renderPage();
    await screen.findByText('GPT-4o');

    await userEvent.setup().click(screen.getByRole('button', { name: 'pick in chat' }));

    await waitFor(() => expect(screen.getByTestId('form-value')).toHaveTextContent('"model_name":"o3-mini"'));
    expect(JSON.parse(screen.getByTestId('form-value').textContent ?? 'null')).toMatchObject({
      model_name: 'o3-mini',
      model_project_id: 17,
    });
    // The form's own picker now shows it too: one setting, two views.
    expect(await screen.findByText('O3 Mini')).toBeInTheDocument();
  });
});
