import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { act, renderHook, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderAgentsRoute } from '../__tests__/testRouter';
import {
  agentWebhookModeOf,
  agentWebhookModeRequest,
  agentWebhookRotateRequest,
  selectableAgentWebhookModes,
  useAgentWebhookTrigger,
} from '../lib/useAgentWebhookTrigger';
import { EditApplicationTriggersPanel } from './EditApplicationTriggersPanel';

/**
 * The agent editor's Triggers section — legacy issue 6656.
 *
 * Driven through msw against the real generated client, with RAW server
 * bodies. The two defects this section can ship are invisible to a shallow
 * render: a credential in the read that runs on every open, and the response
 * envelope read one level too shallow (issue 132), which paints "no trigger"
 * over a configured agent.
 */

const CONFIGURED_TRIGGER = {
  configured: true,
  token_id: 'ab12cd34',
  url: '/api/v2/pipeline_trigger/9/ab12cd34',
  auth_mode: 'token',
  provider: 'custom',
  created_by: 6,
  created_at: '2026-02-01T10:00:00Z',
  last_used_at: '2026-02-02T11:30:00Z',
};

interface Recorded {
  rotated: unknown[];
  revoked: number;
  revealed: number;
  scheduleReads: number;
}

function serve(trigger: Record<string, unknown> = { configured: false }): Recorded {
  const recorded: Recorded = { rotated: [], revoked: 0, revealed: 0, scheduleReads: 0 };
  server.use(
    http.get('*/pipeline_triggers/secret/prompt_lib/:projectId/:versionId', () => {
      recorded.revealed += 1;
      return HttpResponse.json({ ...CONFIGURED_TRIGGER, secret: 'the-secret' });
    }),
    http.get('*/pipeline_triggers/prompt_lib/:projectId/:versionId', () => HttpResponse.json(trigger as Record<string, string | boolean | number>)),
    http.post('*/pipeline_triggers/prompt_lib/:projectId/:versionId', async ({ request }) => {
      const text = await request.text();
      recorded.rotated.push(text === '' ? undefined : JSON.parse(text));
      return HttpResponse.json({ ...CONFIGURED_TRIGGER, secret: 'fresh-secret' });
    }),
    http.delete('*/pipeline_triggers/prompt_lib/:projectId/:versionId', () => {
      recorded.revoked += 1;
      return HttpResponse.json({ ...CONFIGURED_TRIGGER, revoked_at: '2026-02-03T09:00:00Z' });
    }),
    http.get('*/pipeline_schedules/prompt_lib/:projectId/:versionId', () => {
      recorded.scheduleReads += 1;
      return HttpResponse.json({ configured: false, active: false });
    }),
  );
  return recorded;
}

function renderPanel(isReadOnly = false): void {
  renderAgentsRoute(
    <EditApplicationTriggersPanel projectId="9" versionId={1} isReadOnly={isReadOnly} />,
    '/agents/all/42',
    { projectId: '9' },
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('agent webhook modes', () => {
  it('maps each mode to the create/rotate body the server expects', () => {
    expect(agentWebhookModeRequest('custom')).toEqual({ type: 'custom' });
    expect(agentWebhookModeRequest('github')).toEqual({ type: 'github' });
    expect(agentWebhookModeRequest('gitlab')).toEqual({ type: 'gitlab' });
    expect(agentWebhookModeRequest('gitlab_signing')).toEqual({ type: 'gitlab', auth_mode: 'standard_webhooks_hmac' });
    expect(agentWebhookModeRequest('custom_hmac')).toBeUndefined();
  });

  it('rotates in the stored mode with no body, so the server keeps every stored setting', () => {
    const customHmac = { configured: true, auth_mode: 'hmac_sha256' as const, provider: 'custom' as const, signature_header: 'X-Acme-Signature' };
    expect(agentWebhookRotateRequest(customHmac, 'custom_hmac')).toBeUndefined();
    expect(agentWebhookRotateRequest(customHmac, 'github')).toEqual({ type: 'github' });
    expect(agentWebhookRotateRequest({ configured: false }, 'custom')).toEqual({ type: 'custom' });
  });

  it('offers the custom signed mode only to a trigger that already is one', () => {
    expect(selectableAgentWebhookModes('custom').map(mode => mode.value)).not.toContain('custom_hmac');
    expect(selectableAgentWebhookModes('custom_hmac').map(mode => mode.value)).toContain('custom_hmac');
  });

  it('reads the stored row back as the mode', () => {
    expect(agentWebhookModeOf(undefined)).toBe('custom');
    expect(agentWebhookModeOf({ configured: true, auth_mode: 'hmac_sha256', provider: 'github' })).toBe('github');
    expect(agentWebhookModeOf({ configured: true, auth_mode: 'hmac_sha256', signature_header: 'X-Hub-Signature-256' })).toBe('github');
    // A custom-header HMAC trigger minted through the API is NOT GitHub.
    expect(agentWebhookModeOf({ configured: true, auth_mode: 'hmac_sha256', provider: 'custom', signature_header: 'X-Acme-Signature' })).toBe(
      'custom_hmac',
    );
    expect(agentWebhookModeOf({ configured: true, auth_mode: 'standard_webhooks_hmac' })).toBe('gitlab_signing');
    expect(agentWebhookModeOf({ configured: true, auth_mode: 'token', provider: 'gitlab' })).toBe('gitlab');
    expect(agentWebhookModeOf({ configured: true, auth_mode: 'token', provider: 'custom' })).toBe('custom');
  });
});

describe('EditApplicationTriggersPanel', () => {
  it('renders nothing until the agent version is resolved', () => {
    serve();
    renderAgentsRoute(
      <EditApplicationTriggersPanel projectId="9" versionId={undefined} isReadOnly={false} />,
      '/agents/all/42',
      { projectId: '9' },
    );
    expect(screen.queryByTestId('edit-application-triggers-panel')).not.toBeInTheDocument();
  });

  it('says so when the agent has no trigger, and asks for no schedule', async () => {
    const recorded = serve();
    renderPanel();

    expect(await screen.findByTestId('agent-trigger-absent')).toBeInTheDocument();
    expect(screen.getByTestId('agent-trigger-rotate')).toHaveTextContent('Create trigger URL');
    // An agent has the trigger only: the schedule is a pipeline facility.
    expect(recorded.scheduleReads).toBe(0);
  });

  it('shows a configured trigger without its credential', async () => {
    serve(CONFIGURED_TRIGGER);
    renderPanel();

    const url = await screen.findByTestId('agent-trigger-url');
    expect(url).toHaveTextContent(CONFIGURED_TRIGGER.url);
    expect(url.textContent).not.toContain('token=');
    expect(screen.queryByTestId('agent-trigger-secret')).not.toBeInTheDocument();
    expect(screen.queryByTestId('agent-trigger-absent')).not.toBeInTheDocument();
    expect(await screen.findByTestId('agent-trigger-last-used')).toBeInTheDocument();
  });

  it('creates the trigger in the selected mode and shows the new credential once', async () => {
    const user = userEvent.setup();
    const recorded = serve();
    renderPanel();

    await user.click(await screen.findByTestId('agent-trigger-rotate'));
    await waitFor(() => expect(recorded.rotated).toEqual([{ type: 'custom' }]));
    expect(await screen.findByTestId('agent-trigger-secret')).toHaveTextContent('fresh-secret');

    await user.click(screen.getByTestId('agent-trigger-hide'));
    await waitFor(() => expect(screen.queryByTestId('agent-trigger-secret')).not.toBeInTheDocument());
  });

  it('rotates a GitHub trigger as a GitHub trigger', async () => {
    const user = userEvent.setup();
    const recorded = serve({ ...CONFIGURED_TRIGGER, auth_mode: 'hmac_sha256', provider: 'github', signature_header: 'X-Hub-Signature-256' });
    renderPanel();

    await screen.findByTestId('agent-trigger-url');
    await waitFor(() => expect(screen.getByTestId('agent-trigger-mode-hint')).toHaveTextContent('X-Hub-Signature-256'));
    await user.click(screen.getByTestId('agent-trigger-rotate'));
    // No body: the server keeps the stored mode, header, events and opt-in.
    await waitFor(() => expect(recorded.rotated).toEqual([undefined]));
  });

  it('rotates a custom-header HMAC trigger without turning it into a GitHub one', async () => {
    const user = userEvent.setup();
    const recorded = serve({ ...CONFIGURED_TRIGGER, auth_mode: 'hmac_sha256', provider: 'custom', signature_header: 'X-Acme-Signature' });
    renderPanel();

    await waitFor(() => expect(screen.getByTestId('agent-trigger-mode-hint')).toHaveTextContent('X-Acme-Signature'));
    expect(screen.queryByTestId('agent-trigger-mode-pending')).not.toBeInTheDocument();
    await user.click(screen.getByTestId('agent-trigger-rotate'));
    await waitFor(() => expect(recorded.rotated).toEqual([undefined]));
  });

  it('says whose access the runs use, which events start one, and whether callers can change variables', async () => {
    serve({ ...CONFIGURED_TRIGGER, auth_mode: 'hmac_sha256', provider: 'github', events: ['push', 'pull_request'], allow_variable_overrides: true });
    renderPanel();

    expect(await screen.findByTestId('agent-trigger-access')).toHaveTextContent('access of the person who created the trigger');
    expect(await screen.findByTestId('agent-trigger-events')).toHaveTextContent('push, pull_request');
    expect(screen.getByTestId('agent-trigger-variables')).toHaveTextContent('Anyone with this URL can change');
  });

  it('reveals the credential only when asked, and revoking drops it', async () => {
    const user = userEvent.setup();
    const recorded = serve(CONFIGURED_TRIGGER);
    renderPanel();

    await user.click(await screen.findByTestId('agent-trigger-reveal'));
    expect(await screen.findByTestId('agent-trigger-secret')).toHaveTextContent('the-secret');
    expect(recorded.revealed).toBe(1);

    await user.click(screen.getByTestId('agent-trigger-revoke'));
    await waitFor(() => expect(recorded.revoked).toBe(1));
    await waitFor(() => expect(screen.queryByTestId('agent-trigger-secret')).not.toBeInTheDocument());
  });

  it('shows a revoked trigger as revoked, not as absent', async () => {
    serve({ ...CONFIGURED_TRIGGER, revoked_at: '2026-02-03T09:00:00Z' });
    renderPanel();

    expect(await screen.findByTestId('agent-trigger-revoked')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-trigger-absent')).not.toBeInTheDocument();
    expect(screen.queryByTestId('agent-trigger-revoke')).not.toBeInTheDocument();
  });

  it('offers no write to a read-only viewer', async () => {
    serve(CONFIGURED_TRIGGER);
    renderPanel(true);

    await screen.findByTestId('agent-trigger-url');
    expect(screen.getByTestId('agent-trigger-rotate')).toBeDisabled();
    expect(screen.getByTestId('agent-trigger-reveal')).toBeDisabled();
    expect(screen.getByTestId('agent-trigger-revoke')).toBeDisabled();
  });

  it('says so when a write fails', async () => {
    const user = userEvent.setup();
    serve();
    server.use(
      http.post('*/pipeline_triggers/prompt_lib/:projectId/:versionId', () =>
        HttpResponse.json({ error: 'no such pipeline or agent version in this project' }, { status: 404 }),
      ),
    );
    renderPanel();

    await user.click(await screen.findByTestId('agent-trigger-rotate'));
    expect(await screen.findByTestId('agent-triggers-error')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-trigger-secret')).not.toBeInTheDocument();
  });
});

describe('useAgentWebhookTrigger', () => {
  function wrapper({ children }: { readonly children: ReactNode }): ReactNode {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }

  // The panel stays mounted when the editor switches versions. A secret
  // revealed on version A must not stay on screen beside version B's URL.
  it('drops a revealed secret when the version changes', async () => {
    serve(CONFIGURED_TRIGGER);
    const { result, rerender } = renderHook(({ versionId }: { versionId: number }) => useAgentWebhookTrigger('9', versionId), {
      wrapper,
      initialProps: { versionId: 1 },
    });

    await act(async () => {
      await result.current.reveal();
    });
    expect(result.current.secret).toBe('the-secret');

    rerender({ versionId: 2 });
    expect(result.current.secret).toBeUndefined();
    expect(result.current.error).toBeUndefined();

    // Back on version A, A's own credential belongs beside A's URL again.
    rerender({ versionId: 1 });
    expect(result.current.secret).toBe('the-secret');
  });
});
