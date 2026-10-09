/**
 * The `native_client_policy` section's `local_work` group (client contract
 * 1.5, ADR-0029 decision 6).
 *
 * The Configuration page renders a section from the schema the server serves
 * (internal/api/v2/admin/native_client_policy.go `localWorkFields`), so the
 * eleven keys need no page code of their own. What has to hold: every field
 * but the switch is hidden until local work is allowed (`visible_when`), the
 * sandbox mode is a choice of exactly the three modes the server accepts, and
 * a pattern list saves as an array of strings without the blank rows.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminConfiguration } from './Configuration';
import { renderAdminRoute } from './__tests__/testRouter';

const SECTION = 'native_client_policy';
const VISIBLE = { field: 'local_work_allowed', value: true };

const POLICY_SECTION = {
  id: SECTION,
  title: 'Native client policy',
  description: 'What this deployment requires of its mobile and desktop apps.',
  fields: [
    { key: 'local_work_allowed', type: 'boolean', title: 'Allow Local Work', section: SECTION, default: false },
    {
      key: 'local_work_shell',
      type: 'boolean',
      title: 'Allow Shell Commands',
      section: SECTION,
      default: true,
      visible_when: VISIBLE,
    },
    {
      key: 'local_work_max_sandbox_mode',
      type: 'string',
      title: 'Widest Sandbox Mode',
      section: SECTION,
      default: 'workspace-write',
      enum: ['read-only', 'workspace-write', 'full-access'],
      visible_when: VISIBLE,
    },
    {
      key: 'local_work_command_deny',
      type: 'array',
      items: { type: 'string' },
      title: 'Denied Commands',
      section: SECTION,
      default: [],
      maxItems: 200,
      visible_when: VISIBLE,
    },
  ],
};

const STORED = {
  local_work_allowed: false,
  local_work_shell: true,
  local_work_max_sandbox_mode: 'workspace-write',
  local_work_command_deny: [],
};

let writes: { url: string; body: unknown }[] = [];

beforeEach(() => {
  writes = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/plugin_config_schemas/administration', () => HttpResponse.json({ sections: [POLICY_SECTION] })),
    http.get('*/admin/plugin_config_values/administration/:section', () => HttpResponse.json({ values: STORED })),
    http.put('*/admin/plugin_config_values/administration/:section', async ({ request }) => {
      const body = await request.json();
      writes.push({ url: request.url, body });
      return HttpResponse.json({ saved: true, values: { ...STORED, ...(body as { values: object }).values } });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AdminConfiguration — native client local work (client contract 1.5)', () => {
  it('hides the local work fields until local work is allowed', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminConfiguration />);

    const allowed = await screen.findByRole('switch', { name: 'Allow Local Work' });
    expect(allowed).not.toBeChecked();
    expect(screen.queryByRole('switch', { name: 'Allow Shell Commands' })).not.toBeInTheDocument();
    expect(screen.queryByRole('combobox', { name: 'Widest Sandbox Mode' })).not.toBeInTheDocument();

    await user.click(allowed);

    expect(await screen.findByRole('switch', { name: 'Allow Shell Commands' })).toBeChecked();
    expect(screen.getByRole('combobox', { name: 'Widest Sandbox Mode' })).toBeInTheDocument();
  });

  it('offers exactly the three sandbox modes and saves a deny list as strings', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminConfiguration />);

    await user.click(await screen.findByRole('switch', { name: 'Allow Local Work' }));
    await user.click(await screen.findByRole('combobox', { name: 'Widest Sandbox Mode' }));
    const options = within(screen.getByRole('listbox')).getAllByRole('option');
    expect(options.map((option) => option.textContent)).toEqual(['read-only', 'workspace-write', 'full-access']);
    await user.click(within(screen.getByRole('listbox')).getByRole('option', { name: 'read-only' }));

    const addDeny = screen.getByRole('button', { name: /Denied Commands$/ });
    await user.click(addDeny);
    await user.type(screen.getByRole('textbox', { name: 'Denied Commands 1' }), 'rm -rf *');
    // A row added and left blank is dropped at save.
    await user.click(addDeny);

    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(writes).toHaveLength(1);
    });
    expect(writes[0]?.url).toContain(`/administration/${SECTION}`);
    const saved = writes[0]?.body as { values: Record<string, unknown> } | undefined;
    expect(saved?.values).toEqual({
      local_work_allowed: true,
      local_work_max_sandbox_mode: 'read-only',
      local_work_command_deny: ['rm -rf *'],
    });
  });
});
