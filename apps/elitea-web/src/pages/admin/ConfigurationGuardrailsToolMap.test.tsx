/**
 * The Guardrails map fields through the WHOLE page — not the editor alone.
 *
 * `ConfigurationToolMapEditor.test.tsx` drives the editor with a harness that
 * holds `rows` in its own state, while the page stores the field's VALUE (a
 * `{toolkit: [tools]}` map). When the field re-derived rows from that value on
 * every render, a blank row had no key in it, so "Add toolkit" produced a row
 * that vanished on the same render and neither map could be extended from the
 * UI. These tests go through `useAdminConfigurationPage`'s draft and assert the
 * PUT the save produced.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminConfiguration } from './Configuration';
import { renderAdminRoute } from './__tests__/testRouter';

const TOOL_MAP = { type: 'object', additionalProperties: { type: 'array', items: { type: 'string' } } };

const SECTIONS = [
  {
    id: 'guardrails',
    title: 'Guardrails',
    fields: [
      { key: 'sensitive_tools', title: 'Sensitive Action Tools', ...TOOL_MAP },
      { key: 'blocked_tools', title: 'Blocked Tools', ...TOOL_MAP },
    ],
  },
];

let stored: Record<string, unknown> = {};
let puts: Array<{ values: Record<string, unknown> }> = [];

beforeEach(() => {
  stored = { sensitive_tools: {}, blocked_tools: { github: ['delete_repo'] } };
  puts = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/plugin_config_schemas/administration', () => HttpResponse.json({ sections: SECTIONS })),
    http.get('*/admin/plugin_config_values/administration/:section', () =>
      HttpResponse.json({ values: stored }),
    ),
    http.put('*/admin/plugin_config_values/administration/:section', async ({ request }) => {
      const body = (await request.json()) as { values: Record<string, unknown> };
      puts.push(body);
      stored = { ...stored, ...body.values };
      return HttpResponse.json({ saved: true, values: stored });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

async function openGuardrails(): Promise<void> {
  renderAdminRoute(<AdminConfiguration />);
  await screen.findByRole('button', { name: 'Add toolkit — Sensitive Action Tools' });
}

describe('Guardrails map fields — adding a toolkit row on the page', () => {
  it('adds a row to an empty map, and saves the toolkit and tool typed into it', async () => {
    const user = userEvent.setup();
    await openGuardrails();
    expect(screen.getAllByLabelText('Toolkit type')).toHaveLength(1); // blocked_tools' github row

    await user.click(screen.getByRole('button', { name: 'Add toolkit — Sensitive Action Tools' }));

    const toolkitInputs = screen.getAllByLabelText('Toolkit type');
    expect(toolkitInputs).toHaveLength(2);
    // Sensitive Action Tools renders first, so its new row is the first input.
    await user.type(toolkitInputs[0] as HTMLElement, 'mcp');
    await user.type(screen.getAllByLabelText('Tools')[0] as HTMLElement, 'echo{Enter}');

    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(puts).toHaveLength(1);
    });
    expect(puts[0]?.values).toEqual({ sensitive_tools: { mcp: ['echo'] } });
  });

  it('adds a second row to a populated map without re-sorting it under the cursor', async () => {
    // `github` is already stored. Typing `atlassian` into a new row below it
    // used to re-sort the map on the first keystroke, moving the half-typed
    // key above `github` while the focused input (by index) showed `github`.
    const user = userEvent.setup();
    await openGuardrails();

    await user.click(screen.getByRole('button', { name: 'Add toolkit — Blocked Tools' }));
    const toolkitInputs = screen.getAllByLabelText('Toolkit type');
    expect(toolkitInputs).toHaveLength(2);
    expect(toolkitInputs[0]).toHaveValue('github');

    await user.type(toolkitInputs[1] as HTMLElement, 'atlassian');
    expect(screen.getAllByLabelText('Toolkit type').map((input) => (input as HTMLInputElement).value)).toEqual([
      'github',
      'atlassian',
    ]);
    await user.type(screen.getAllByLabelText('Tools')[1] as HTMLElement, 'delete_page{Enter}');

    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(puts).toHaveLength(1);
    });
    expect(puts[0]?.values).toEqual({
      blocked_tools: { github: ['delete_repo'], atlassian: ['delete_page'] },
    });
  });

  it('keeps an added row that is still blank, and Discard removes it', async () => {
    const user = userEvent.setup();
    await openGuardrails();

    await user.click(screen.getByRole('button', { name: 'Add toolkit — Blocked Tools' }));
    await user.click(screen.getByRole('button', { name: 'Add toolkit — Blocked Tools' }));
    expect(screen.getAllByLabelText('Toolkit type')).toHaveLength(3);

    // Two blank rows are not a conflict and not yet a change worth saving.
    expect(screen.queryByText(/Another row already covers this toolkit/)).not.toBeInTheDocument();

    await user.type(screen.getAllByLabelText('Toolkit type')[1] as HTMLElement, 'jira');
    await user.click(screen.getByRole('button', { name: 'Discard' }));
    await waitFor(() => {
      expect(screen.getAllByLabelText('Toolkit type')).toHaveLength(1);
    });
    expect(screen.getAllByLabelText('Toolkit type')[0]).toHaveValue('github');
  });

  it('shows the stored map after a save, dropping a row that was never named', async () => {
    // The refetch after a save is a value the editor did not emit, so the rows
    // are re-read from it: sorted, and without the blank in-progress row.
    const user = userEvent.setup();
    await openGuardrails();

    await user.click(screen.getByRole('button', { name: 'Add toolkit — Blocked Tools' }));
    await user.click(screen.getByRole('button', { name: 'Add toolkit — Blocked Tools' }));
    await user.type(screen.getAllByLabelText('Toolkit type')[1] as HTMLElement, 'atlassian');
    expect(screen.getAllByLabelText('Toolkit type')).toHaveLength(3);

    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(screen.getAllByLabelText('Toolkit type')).toHaveLength(2);
    });
    expect(screen.getAllByLabelText('Toolkit type').map((input) => (input as HTMLInputElement).value)).toEqual([
      'atlassian',
      'github',
    ]);
    expect(puts[0]?.values).toEqual({ blocked_tools: { github: ['delete_repo'], atlassian: [] } });
  });

  it('flags a duplicate typed into a new row instead of silently merging it', async () => {
    // On a map value two `github` keys are one key, so the warning the editor
    // exists to show could never appear on the page.
    const user = userEvent.setup();
    await openGuardrails();

    await user.click(screen.getByRole('button', { name: 'Add toolkit — Blocked Tools' }));
    await user.type(screen.getAllByLabelText('Toolkit type')[1] as HTMLElement, 'GitHub');

    expect(screen.getAllByLabelText('Toolkit type')).toHaveLength(2);
    expect(screen.getByText(/Another row already covers this toolkit/)).toBeVisible();
  });
});
