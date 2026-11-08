/**
 * The lead agent (ADR-0050 decision 7d, issue 1612): the "Lead" badge and the
 * row menu that makes an agent the lead or clears it. Updating a
 * conversation replaces its whole meta, so the write must keep every other
 * key (`metaWithLead`).
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';
import { act, fireEvent, render, renderHook, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AgentLeadControl, hasSeveralAgents, useConversationLead } from './ChatAgentLead';

const BASE = '/api/v2';
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function wrapper({ children }: { readonly children: ReactNode }): ReactNode {
  return <QueryClientProvider client={new QueryClient()}>{children}</QueryClientProvider>;
}

beforeEach(() => { configureGeneratedClient({ baseUrl: BASE }); });
afterEach(() => { resetGeneratedClient(); });

describe('useConversationLead', () => {
  it('writes the lead into the meta it was given, keeping its other keys, and clears it with 0', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.put(`${BASE}/elitea_core/conversation/prompt_lib/77/5`, async ({ request }) => {
        const body = await request.json();
        bodies.push(body);
        return HttpResponse.json({ id: 5, name: 'Conversation', meta: (body as { meta?: unknown }).meta });
      }),
    );
    const meta = { steps_limit: 25, internal_tools: ['planner'] };
    const { result, rerender } = renderHook(
      ({ current }: { current: Record<string, unknown> }) => useConversationLead({ projectId: '77', conversationId: 5, meta: current }),
      { wrapper, initialProps: { current: meta } },
    );
    expect(result.current.leadParticipantId).toBe(0);

    await act(async () => { await result.current.setLead(13); });
    expect(bodies[0]).toEqual({ meta: { steps_limit: 25, internal_tools: ['planner'], floor: { policy: 'lead', lead_participant_id: 13 } } });
    expect(result.current.leadParticipantId).toBe(13);

    const withLead = { ...meta, floor: { policy: 'lead', lead_participant_id: 13 } };
    rerender({ current: withLead });
    expect(result.current.leadParticipantId).toBe(13);

    await act(async () => { await result.current.setLead(0); });
    expect(bodies[1]).toEqual({ meta: { steps_limit: 25, internal_tools: ['planner'] } });
    await waitFor(() => { expect(result.current.leadParticipantId).toBe(0); });
  });
});

describe('AgentLeadControl', () => {
  function renderControl(participantId: number, leadParticipantId: number, onSetLead = vi.fn()) {
    render(
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <AgentLeadControl participantId={participantId} leadParticipantId={leadParticipantId} onSetLead={onSetLead} />
      </ThemeProvider>,
    );
    return onSetLead;
  }

  it('makes a non-lead agent the lead', () => {
    const onSetLead = renderControl(12, 13);
    expect(screen.queryByTestId('participant-lead-badge')).toBeNull();
    fireEvent.click(screen.getByTestId('participant-lead-menu'));
    fireEvent.click(screen.getByTestId('participant-lead-make'));
    expect(onSetLead).toHaveBeenCalledWith(12);
  });

  it('badges the lead and clears it', () => {
    const onSetLead = renderControl(13, 13);
    expect(screen.getByTestId('participant-lead-badge')).toHaveTextContent('Lead');
    fireEvent.click(screen.getByTestId('participant-lead-menu'));
    fireEvent.click(screen.getByTestId('participant-lead-clear'));
    expect(onSetLead).toHaveBeenCalledWith(0);
  });

  it('is offered only with two or more agents', () => {
    expect(hasSeveralAgents([{ entity_name: 'application' }, { entity_name: 'user' }])).toBe(false);
    expect(hasSeveralAgents([{ entity_name: 'application' }, { entity_name: 'application' }])).toBe(true);
  });
});
