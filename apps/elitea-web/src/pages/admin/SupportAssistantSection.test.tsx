/**
 * Focused coverage for the Support Assistant section on `pages/admin/Features.tsx`.
 *
 * `Features.test.tsx` proves the section RENDERS with its enable switch. This
 * file proves the operator can actually configure the assistant: turn it on,
 * name it, and pick the agent that answers — the fields `config_schemas.go`'s
 * `supportAssistantSection()` declares beyond the one switch. The real server
 * schema carries `support_assistant_name` (the assistant's title) and
 * `support_agent_id` (the agent it hands questions to), and this suite pins
 * both.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminFeatures } from './Features';
import { renderAdminRoute } from './__tests__/testRouter';

interface RecordedWrite {
  readonly url: string;
  readonly body: { values?: Record<string, unknown> };
}

/** One section, shaped like the server's real `supportAssistantSection()`. */
const SECTIONS = [
  {
    id: 'support_assistant',
    page: 'features',
    title: 'Support Assistant',
    description: 'Enable the in-app support assistant widget for all users.',
    always_visible: true,
    fields: [
      {
        key: 'support_assistant_enabled',
        type: 'boolean',
        title: 'Assistant Enabled',
        description: 'When enabled, the support assistant widget is available to all users.',
        default: false,
      },
      {
        key: 'support_agent_id',
        type: 'integer',
        title: 'Agent ID',
        description: 'Application ID of the support agent.',
        default: null,
      },
      {
        key: 'support_assistant_name',
        type: 'string',
        title: 'Assistant Name',
        description: 'Display name for the support assistant.',
        default: 'ELITEA Support',
      },
    ],
  },
];

let writes: RecordedWrite[] = [];
let storedValues: Record<string, unknown> = {};

function useHandlers(): void {
  server.use(
    http.get('*/admin/plugin_config_schemas/administration', () => HttpResponse.json({ sections: SECTIONS })),
    http.get('*/admin/plugin_config_values/administration/:section', () =>
      HttpResponse.json({ values: storedValues }),
    ),
    http.put('*/admin/plugin_config_values/administration/:section', async ({ request }) => {
      const body = (await request.json()) as { values?: Record<string, unknown> };
      writes.push({ url: request.url, body });
      return HttpResponse.json({ saved: true, values: {} });
    }),
  );
}

function lastWrite(): Record<string, unknown> {
  return writes.at(-1)?.body.values ?? {};
}

async function renderSection(): Promise<void> {
  renderAdminRoute(<AdminFeatures />);
  await screen.findByRole('switch', { name: 'Assistant Enabled' });
}

beforeEach(() => {
  writes = [];
  storedValues = {
    support_assistant_enabled: false,
    support_agent_id: null,
    support_assistant_name: 'ELITEA Support',
  };
  configureGeneratedClient({ baseUrl: '/api/v2' });
  useHandlers();
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AdminFeatures — Support Assistant enable/disable', () => {
  it('starts OFF when the server reports the flag off', async () => {
    await renderSection();
    expect(screen.getByRole('switch', { name: 'Assistant Enabled' })).not.toBeChecked();
  });

  it('turns the assistant ON and saves only that field', async () => {
    await renderSection();

    await userEvent.click(screen.getByRole('switch', { name: 'Assistant Enabled' }));
    expect(screen.getByRole('switch', { name: 'Assistant Enabled' })).toBeChecked();

    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect(lastWrite()).toEqual({ support_assistant_enabled: true });
  });

  it('turns an already-enabled assistant OFF', async () => {
    storedValues = { ...storedValues, support_assistant_enabled: true };
    await renderSection();

    const toggle = await screen.findByRole('switch', { name: 'Assistant Enabled' });
    expect(toggle).toBeChecked();

    await userEvent.click(toggle);
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect(lastWrite()).toEqual({ support_assistant_enabled: false });
  });
});

describe('AdminFeatures — Support Assistant title and agent', () => {
  it('shows the configured assistant name', async () => {
    await renderSection();
    expect(screen.getByRole('textbox', { name: 'Assistant Name' })).toHaveValue('ELITEA Support');
  });

  it('renames the assistant and saves the new title', async () => {
    await renderSection();

    const nameField = screen.getByRole('textbox', { name: 'Assistant Name' });
    await userEvent.clear(nameField);
    await userEvent.type(nameField, 'Acme Help Desk');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect(lastWrite()).toEqual({ support_assistant_name: 'Acme Help Desk' });
  });

  it('shows no agent configured when the id is unset', async () => {
    await renderSection();
    expect(screen.getByRole('spinbutton', { name: 'Agent ID' })).toHaveValue(null);
  });

  it('assigns the agent that answers, as a NUMBER on the wire', async () => {
    await renderSection();

    const agentField = screen.getByRole('spinbutton', { name: 'Agent ID' });
    await userEvent.type(agentField, '42');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    // A string "42" would be stored and then rejected by the server's numeric
    // check — the same class of defect the Agent Publishing suite pins for
    // `publish_whitelist_project_ids`.
    expect(lastWrite()).toEqual({ support_agent_id: 42 });
  });

  it('clears a configured agent id back to NULL rather than zero', async () => {
    storedValues = { ...storedValues, support_agent_id: 7 };
    await renderSection();

    const agentField = await screen.findByRole('spinbutton', { name: 'Agent ID' });
    expect(agentField).toHaveValue(7);

    await userEvent.clear(agentField);
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect(lastWrite()).toEqual({ support_agent_id: null });
  });
});
