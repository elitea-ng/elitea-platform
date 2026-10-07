/**
 * The `native_client_policy` section's data controls (client contract 1.3).
 *
 * The Configuration page renders a section from the schema the server serves
 * (internal/api/v2/admin/native_client_policy.go), so the six new keys need
 * no page code of their own. What has to hold is that they are EDITABLE the
 * way the server validates them: the five booleans as switches, and
 * `notification_preview` as a choice of exactly `none` / `title` (the
 * server refuses anything else). The test serves the section as the server
 * declares it and asserts the request a change produces.
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

const POLICY_SECTION = {
  id: SECTION,
  title: 'Native client policy',
  description: 'What this deployment requires of its mobile and desktop apps.',
  fields: [
    { key: 'require_device_lock', type: 'boolean', title: 'Require Device Lock', section: SECTION, default: false },
    { key: 'allow_share_out', type: 'boolean', title: 'Allow Copy, Share and Export', section: SECTION, default: true },
    { key: 'allow_share_in', type: 'boolean', title: 'Allow Sharing Into the App', section: SECTION, default: true },
    { key: 'allow_cloud_stt', type: 'boolean', title: 'Allow Cloud Speech Recognition', section: SECTION, default: false },
    {
      key: 'notification_preview',
      type: 'string',
      title: 'Notification Preview',
      section: SECTION,
      default: 'none',
      enum: ['none', 'title'],
    },
    {
      key: 'allow_notification_actions',
      type: 'boolean',
      title: 'Allow Notification Actions',
      section: SECTION,
      default: true,
    },
    {
      key: 'allow_system_surfaces',
      type: 'boolean',
      title: 'Show Titles on Widgets and Quick Actions',
      section: SECTION,
      default: false,
    },
  ],
};

const STORED = {
  require_device_lock: false,
  allow_share_out: true,
  allow_share_in: true,
  allow_cloud_stt: false,
  notification_preview: 'none',
  allow_notification_actions: true,
  allow_system_surfaces: false,
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

describe('AdminConfiguration — native client data controls (client contract 1.3)', () => {
  it('renders each boolean as a switch at its stored value', async () => {
    renderAdminRoute(<AdminConfiguration />);

    expect(await screen.findByRole('switch', { name: 'Allow Copy, Share and Export' })).toBeChecked();
    expect(screen.getByRole('switch', { name: 'Allow Sharing Into the App' })).toBeChecked();
    expect(screen.getByRole('switch', { name: 'Allow Cloud Speech Recognition' })).not.toBeChecked();
    expect(screen.getByRole('switch', { name: 'Allow Notification Actions' })).toBeChecked();
    expect(screen.getByRole('switch', { name: 'Show Titles on Widgets and Quick Actions' })).not.toBeChecked();
  });

  it('offers exactly none and title for the preview, and saves only what changed', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminConfiguration />);

    await user.click(await screen.findByRole('switch', { name: 'Allow Copy, Share and Export' }));
    await user.click(screen.getByRole('combobox', { name: 'Notification Preview' }));
    const options = within(screen.getByRole('listbox')).getAllByRole('option');
    expect(options.map((option) => option.textContent)).toEqual(['none', 'title']);
    await user.click(within(screen.getByRole('listbox')).getByRole('option', { name: 'title' }));
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(writes).toHaveLength(1);
    });
    expect(writes[0]?.url).toContain(`/administration/${SECTION}`);
    const saved = writes[0]?.body as { values: Record<string, unknown> } | undefined;
    expect(saved?.values).toEqual({
      allow_share_out: false,
      notification_preview: 'title',
    });
  });
});
