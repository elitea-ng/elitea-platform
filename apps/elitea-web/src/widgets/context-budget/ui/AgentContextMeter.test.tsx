import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { AgentContextMeter } from './AgentContextMeter';

const BASE = '/api/v2';
const STATUS_URL = `${BASE}/elitea_core/context_analytics/prompt_lib/7/42`;

function wrap(ui: ReactElement): ReactElement {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={client}>{ui}</QueryClientProvider>;
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('AgentContextMeter', () => {
  it("shows each agent's own occupancy, and says when it was summarised while idle", async () => {
    server.use(http.get(STATUS_URL, () => HttpResponse.json({
      current_tokens: 1, max_tokens: 2,
      agent_contexts: [
        { participant_id: 5, current_tokens: 116000, max_tokens: 128000, idle_compacted: false, active: false },
        { participant_id: 6, current_tokens: 8000, max_tokens: 200000, idle_compacted: true, active: false },
      ],
    })));
    const { findByTestId } = renderWithTheme(wrap(
      <>
        <AgentContextMeter projectId="7" conversationId="42" participantId={5} />
        <AgentContextMeter projectId="7" conversationId="42" participantId={6} />
      </>,
    ));
    const first = await findByTestId('agent-context-meter-5');
    expect(first.dataset.percentage).toBe('91');
    expect(first.textContent).toContain('91%');
    const second = await findByTestId('agent-context-meter-6');
    expect(second.dataset.idleCompacted).toBe('true');
    expect(second.textContent).toContain('Summarised while idle');
  });

  it('renders nothing for an agent that has not answered', async () => {
    server.use(http.get(STATUS_URL, () => HttpResponse.json({ agent_contexts: [] })));
    const { container } = renderWithTheme(wrap(<AgentContextMeter projectId="7" conversationId="42" participantId={5} />));
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(container.querySelector('[data-testid^="agent-context-meter"]')).toBeNull();
  });
});
