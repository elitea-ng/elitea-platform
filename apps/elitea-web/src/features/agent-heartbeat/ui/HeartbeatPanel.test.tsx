import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { AgentHeartbeatSettings } from '@/shared/api/generated/model';
import { server } from '@/test/setup';
import { renderWithQueryAndTheme } from '@/test/renderWithQueryAndTheme';

import { HeartbeatPanel } from './HeartbeatPanel';

const BASE = '/api/v2';
const SETTINGS = `${BASE}/elitea_core/agent_heartbeat/prompt_lib/:projectId/:applicationId`;
const HISTORY = `${BASE}/elitea_core/agent_heartbeats/prompt_lib/:projectId/:applicationId`;

function settings(overrides: Partial<AgentHeartbeatSettings> = {}): AgentHeartbeatSettings {
  return {
    application_id: 31,
    configured: false,
    enabled: false,
    owner_user_id: 0,
    mandate_id: null,
    home_conversation_uuid: null,
    release_id: null,
    next_wake_at: '2026-11-09T15:00:00Z',
    last_wake_at: null,
    paused_until: null,
    last_skip_reason: '',
    updated_at: '2026-11-09T14:00:00Z',
    governance: {
      platform_enabled: true,
      project_enabled: true,
      tier: 'medium',
      personal: false,
      can_enable: true,
      wakes_per_day: 24,
      acts_per_day: 4,
      spend_per_day_usd: 2,
    },
    release: { release_id: 5, version_id: 42, mode: 'heartbeat', interval: '1h0m0s', act_target: 'home_thread', goal_count: 1 },
    ...overrides,
  };
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('HeartbeatPanel', () => {
  it('asks to save the agent first', () => {
    renderWithQueryAndTheme(<HeartbeatPanel projectId="1" applicationId={undefined} />);
    expect(screen.getByTestId('agent-heartbeat-unsaved')).toBeInTheDocument();
  });

  it('turns the heartbeat on and lists the history', async () => {
    let saved: unknown;
    server.use(
      http.get(SETTINGS, () => HttpResponse.json(settings())),
      http.put(SETTINGS, async ({ request }) => {
        saved = await request.json();
        return HttpResponse.json(settings({ configured: true, enabled: true, owner_user_id: 11 }));
      }),
      http.get(HISTORY, () =>
        HttpResponse.json({
          total: 1,
          rows: [
            {
              id: 1, wake_id: 'w', woke_at: '2026-11-09T14:00:00Z', outcome: 'act_degraded', reason_code: 'mandate_required',
              reason: 'The build failed.', release_id: 5, digest_sha256: null, triage_spend_usd: 0, execution_id: null,
              conversation_uuid: null, paused_until: null,
            },
          ],
        }),
      ),
    );
    renderWithQueryAndTheme(<HeartbeatPanel projectId="1" applicationId={31} />);
    expect(await screen.findByTestId('agent-heartbeat-release')).toHaveTextContent('Every 1h0m0s');
    expect(await screen.findByText('act_degraded (mandate_required)')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('switch', { name: 'Heartbeat on' }));
    await waitFor(() => expect(saved).toEqual({ enabled: true }));
    expect(await screen.findByTestId('agent-heartbeat-no-mandate')).toBeInTheDocument();
  });

  it('explains why the heartbeat cannot be turned on', async () => {
    server.use(
      http.get(SETTINGS, () =>
        HttpResponse.json(
          settings({
            governance: { ...settings().governance, tier: 'low', can_enable: false, reason: 'proactivity_tier_too_low' },
          }),
        ),
      ),
      http.get(HISTORY, () => HttpResponse.json({ total: 0, rows: [] })),
    );
    renderWithQueryAndTheme(<HeartbeatPanel projectId="1" applicationId={31} />);
    expect(await screen.findByTestId('agent-heartbeat-reason')).toHaveTextContent('at least medium risk');
    expect(screen.getByRole('switch', { name: 'Heartbeat on' })).toBeDisabled();
  });

  it('shows the server refusal of a mandate', async () => {
    server.use(
      http.get(SETTINGS, () => HttpResponse.json(settings({ configured: true, enabled: true, owner_user_id: 11 }))),
      http.put(SETTINGS, () =>
        HttpResponse.json(
          { error: 'The mandate must be a delegated mandate of the heartbeat owner.', code: 'proactivity_mandate_not_owner' },
          { status: 422 },
        ),
      ),
      http.get(HISTORY, () => HttpResponse.json({ total: 0, rows: [] })),
    );
    renderWithQueryAndTheme(<HeartbeatPanel projectId="1" applicationId={31} />);
    await userEvent.type(await screen.findByTestId('agent-heartbeat-mandate'), '7d1f6a52-0c55-4d5e-9a3e-6f0b1c2d3e4f');
    await userEvent.click(screen.getByTestId('agent-heartbeat-save-mandate'));
    expect(await screen.findByTestId('agent-heartbeat-refusal')).toHaveTextContent('delegated mandate');
  });
});
