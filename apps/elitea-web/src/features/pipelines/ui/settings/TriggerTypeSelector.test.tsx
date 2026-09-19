import userEvent from '@testing-library/user-event';
import { waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { resetBackendCapabilitiesForTests, setBackendCapabilityForTests } from '@/shared/config/backendCapabilities';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithRouterAndProject } from '../../__tests__/testUtils';
import { TriggerTypeSelector } from './TriggerTypeSelector';

const BASE = '/api/v2';
const PROJECT_ID = '1';
const VERSION_ID = 7;
const SCHEDULE_URL = `${BASE}/pipeline_schedules/prompt_lib/${PROJECT_ID}/${VERSION_ID}`;
const TRIGGER_URL = `${BASE}/pipeline_triggers/prompt_lib/${PROJECT_ID}/${VERSION_ID}`;
const REVEAL_URL = `${BASE}/pipeline_triggers/secret/prompt_lib/${PROJECT_ID}/${VERSION_ID}`;

/** The "this pipeline has neither" answer both reads give — a 200, never a 404. */
function serveNothingConfigured(): void {
  server.use(
    http.get(SCHEDULE_URL, () => HttpResponse.json({ configured: false, active: false })),
    http.get(TRIGGER_URL, () => HttpResponse.json({ configured: false })),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  setBackendCapabilityForTests('pipelineTriggers', true);
});

afterEach(() => {
  resetGeneratedClient();
  resetBackendCapabilitiesForTests();
});

describe('TriggerTypeSelector', () => {
  /**
   * Regression pin (#899, third root cause): without React Flow's `nopan
   * nodrag` escape hatch on a wrapper ABOVE the select, the canvas's drag
   * layer eats the mouse-down and the menu never opens on the real canvas —
   * a failure no unit-level click can reproduce, because jsdom has no canvas.
   */
  it('shields the dropdown from the canvas drag layer', async () => {
    serveNothingConfigured();
    const { findByRole } = renderWithRouterAndProject(
      <TriggerTypeSelector
        projectId={PROJECT_ID}
        versionId={VERSION_ID}
      />,
      PROJECT_ID,
    );
    const select = await findByRole('combobox');
    expect(select.closest('.nodrag'), 'the Trigger select must sit inside a `nodrag` ancestor').not.toBeNull();
    expect(select.closest('.nopan')).not.toBeNull();
  });

  it('defaults to Chat Message and shows all three trigger options', async () => {
    serveNothingConfigured();

    const { findByText } = renderWithRouterAndProject(
      <TriggerTypeSelector
        projectId={PROJECT_ID}
        versionId={VERSION_ID}
      />,
      PROJECT_ID,
    );

    expect(await findByText('Trigger')).toBeInTheDocument();
    expect(await findByText('Chat Message')).toBeInTheDocument();
  });

  it('restricts to Chat Message only when the saved YAML has interactive elements', async () => {
    serveNothingConfigured();

    const versionInstructions = 'nodes:\n  - id: HITL 1\n    type: hitl\n';

    const { findByRole } = renderWithRouterAndProject(
      <TriggerTypeSelector
        projectId={PROJECT_ID}
        versionId={VERSION_ID}
        versionInstructions={versionInstructions}
      />,
      PROJECT_ID,
    );

    const select = await findByRole('combobox');
    await userEvent.setup().click(select);
    // Only the Chat Message option is offered — no Schedule/Webhook rows exist.
    expect(document.querySelectorAll('[data-value="schedule"]').length).toBe(0);
    expect(document.querySelectorAll('[data-value="webhook"]').length).toBe(0);
  });

  /** The capability now gates only whether this BUILD serves the two facilities (#899). */
  it('offers Chat Message only while the capability is off', async () => {
    setBackendCapabilityForTests('pipelineTriggers', false);
    const { findByRole } = renderWithRouterAndProject(
      <TriggerTypeSelector
        projectId={PROJECT_ID}
        versionId={VERSION_ID}
      />,
      PROJECT_ID,
    );

    const select = await findByRole('combobox');
    await userEvent.setup().click(select);
    expect(document.querySelectorAll('[data-value="schedule"]').length).toBe(0);
    expect(document.querySelectorAll('[data-value="webhook"]').length).toBe(0);
  });

  it('opens the schedule modal when Schedule is selected', async () => {
    serveNothingConfigured();
    const user = userEvent.setup();

    const { findByRole, getByRole, findByText } = renderWithRouterAndProject(
      <TriggerTypeSelector
        projectId={PROJECT_ID}
        versionId={VERSION_ID}
      />,
      PROJECT_ID,
    );

    await user.click(await findByRole('combobox'));
    await user.click(getByRole('option', { name: 'Schedule' }));

    expect(await findByText('Schedule settings')).toBeInTheDocument();
  });

  it('creates the inbound trigger and opens the webhook modal when Webhook is selected', async () => {
    serveNothingConfigured();
    let rotated = false;
    server.use(
      http.post(TRIGGER_URL, () => {
        rotated = true;
        return HttpResponse.json({ configured: true, token_id: 'tok', url: '/api/v2/pipeline_trigger/1/tok', secret: 'sec-123' });
      }),
    );
    const user = userEvent.setup();

    const { findByRole, getByRole, findByText, findByTestId } = renderWithRouterAndProject(
      <TriggerTypeSelector
        projectId={PROJECT_ID}
        versionId={VERSION_ID}
      />,
      PROJECT_ID,
    );

    await user.click(await findByRole('combobox'));
    await user.click(getByRole('option', { name: 'Webhook' }));

    expect(await findByText('Webhook settings')).toBeInTheDocument();
    await waitFor(() => expect(rotated).toBe(true));
    // The credential the create answered is shown, masked until revealed.
    expect(await findByTestId('pipeline-webhook-secret')).toBeInTheDocument();
  });

  it('lists a configured schedule and a configured webhook side by side', async () => {
    server.use(
      http.get(SCHEDULE_URL, () => HttpResponse.json({ configured: true, active: true, cron: '0 9 * * 1' })),
      http.get(TRIGGER_URL, () => HttpResponse.json({ configured: true, token_id: 'tok', url: '/api/v2/pipeline_trigger/1/tok' })),
    );

    const { findByTestId } = renderWithRouterAndProject(
      <TriggerTypeSelector
        projectId={PROJECT_ID}
        versionId={VERSION_ID}
      />,
      PROJECT_ID,
    );

    expect(await findByTestId('pipeline-trigger-row-schedule')).toHaveTextContent('0 9 * * 1');
    expect(await findByTestId('pipeline-trigger-row-webhook')).toBeInTheDocument();
  });

  it('deletes the schedule from its own row', async () => {
    let deleted = false;
    server.use(
      http.get(SCHEDULE_URL, () => HttpResponse.json({ configured: true, active: true, cron: '0 9 * * 1' })),
      http.get(TRIGGER_URL, () => HttpResponse.json({ configured: false })),
      http.delete(SCHEDULE_URL, () => {
        deleted = true;
        return HttpResponse.json({ configured: false, active: false });
      }),
    );
    const user = userEvent.setup();

    const { findByTestId } = renderWithRouterAndProject(
      <TriggerTypeSelector
        projectId={PROJECT_ID}
        versionId={VERSION_ID}
      />,
      PROJECT_ID,
    );

    await user.click(await findByTestId('pipeline-trigger-delete-schedule'));
    await waitFor(() => expect(deleted).toBe(true));
  });

  it('reveals the webhook secret through the dedicated write-permission operation', async () => {
    server.use(
      http.get(SCHEDULE_URL, () => HttpResponse.json({ configured: false, active: false })),
      http.get(TRIGGER_URL, () => HttpResponse.json({ configured: true, token_id: 'tok', url: '/api/v2/pipeline_trigger/1/tok' })),
      http.get(REVEAL_URL, () => HttpResponse.json({ configured: true, token_id: 'tok', url: '/api/v2/pipeline_trigger/1/tok', secret: 'revealed-secret' })),
    );
    const user = userEvent.setup();

    const { findByTestId, getByTestId } = renderWithRouterAndProject(
      <TriggerTypeSelector
        projectId={PROJECT_ID}
        versionId={VERSION_ID}
      />,
      PROJECT_ID,
    );

    await user.click(await findByTestId('pipeline-trigger-edit-webhook'));
    // The plain read never carries the credential, so nothing is shown yet.
    expect(document.querySelector('[data-testid="pipeline-webhook-secret"]')).toBeNull();

    await user.click(getByTestId('pipeline-webhook-reveal'));
    const secret = await findByTestId('pipeline-webhook-secret');
    expect(secret).toBeInTheDocument();
    // Masked until the eye is clicked — the reveal is a credential, not a label.
    expect(secret).toHaveValue('•'.repeat('revealed-secret'.length));
  });

  it('surfaces the backend error text (not a fixed generic message) when a write fails', async () => {
    // Regression coverage (confirmed finding 3): this used to always report
    // the fixed 'Failed to ...' string regardless of what the backend actually
    // returned -- discarding the real `{"error": "boom"}` envelope's message.
    server.use(
      http.get(SCHEDULE_URL, () => HttpResponse.json({ configured: true, active: true, cron: '0 0 * * 6' })),
      http.get(TRIGGER_URL, () => HttpResponse.json({ configured: false })),
      http.delete(SCHEDULE_URL, () => HttpResponse.json({ error: 'boom' }, { status: 400 })),
    );
    const user = userEvent.setup();
    const onNotifyError = vi.fn();

    const { findByRole, getByRole } = renderWithRouterAndProject(
      <TriggerTypeSelector
        projectId={PROJECT_ID}
        versionId={VERSION_ID}
        onNotifyError={onNotifyError}
      />,
      PROJECT_ID,
    );

    await user.click(await findByRole('combobox'));
    await user.click(getByRole('option', { name: 'Chat Message' }));

    await waitFor(() => expect(onNotifyError).toHaveBeenCalledWith('boom'));
  });

  it('does not throw when projectId/versionId are undefined', async () => {
    const { findByText } = renderWithRouterAndProject(<TriggerTypeSelector />, undefined);
    expect(await findByText('Trigger')).toBeInTheDocument();
  });
});
