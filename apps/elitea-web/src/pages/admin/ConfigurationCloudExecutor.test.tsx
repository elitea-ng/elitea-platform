/**
 * The `cloud_executor` section's two switches (ADR-0034 decision 11, owner
 * decision 2026-10-19): "Enable the Cloud Executor" and, right after it,
 * "Default for Projects", what a project that has not chosen gets.
 *
 * The page renders the section from the schema the server serves
 * (internal/api/v2/admin/cloud_executor.go), so the fields below are that
 * descriptor's first two, transcribed. What has to hold: the default is its
 * own switch beside Enabled, it shows the stored value, and a save sends it
 * as a boolean under its own key.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminConfiguration } from './Configuration';
import { renderAdminRoute } from './__tests__/testRouter';

const SECTION = 'cloud_executor';

const CLOUD_EXECUTOR_SECTION = {
  id: SECTION,
  title: 'Cloud executor',
  description: 'Whether desktop Local work threads can be sent to a cloud executor, and its limits.',
  always_visible: true,
  fields: [
    {
      key: 'enabled',
      type: 'boolean',
      title: 'Enable the Cloud Executor',
      description: 'Allow Send to cloud from the desktop.',
      section: SECTION,
      default: false,
    },
    {
      key: 'project_default_enabled',
      type: 'boolean',
      title: 'Default for Projects',
      description:
        'Whether a project that has not turned the cloud executor on or off has it on. Existing projects follow this default too.',
      section: SECTION,
      default: false,
    },
  ],
};

let stored: Record<string, unknown> = {};
let writes: { url: string; body: unknown }[] = [];

beforeEach(() => {
  writes = [];
  stored = { enabled: true, project_default_enabled: false };
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/plugin_config_schemas/administration', () =>
      HttpResponse.json({ sections: [CLOUD_EXECUTOR_SECTION] }),
    ),
    http.get('*/admin/plugin_config_values/administration/:section', () => HttpResponse.json({ values: stored })),
    http.put('*/admin/plugin_config_values/administration/:section', async ({ request }) => {
      const body = await request.json();
      writes.push({ url: request.url, body });
      stored = { ...stored, ...(body as { values: object }).values };
      return HttpResponse.json({ saved: true, values: stored });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AdminConfiguration — cloud executor default for projects', () => {
  it('shows the project default as its own switch right after Enabled', async () => {
    renderAdminRoute(<AdminConfiguration />);

    const enabled = await screen.findByRole('switch', { name: 'Enable the Cloud Executor' });
    const projectDefault = screen.getByRole('switch', { name: 'Default for Projects' });
    expect(enabled).toBeChecked();
    expect(projectDefault).not.toBeChecked();

    // Next to Enabled: the very next switch on the page.
    const switches = screen.getAllByRole('switch');
    expect(switches.indexOf(projectDefault)).toBe(switches.indexOf(enabled) + 1);
  });

  it('saves the project default as a boolean under its own key', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminConfiguration />);

    await user.click(await screen.findByRole('switch', { name: 'Default for Projects' }));
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(writes).toHaveLength(1);
    });
    expect(writes[0]?.url).toContain(`/administration/${SECTION}`);
    const saved = writes[0]?.body as { values: Record<string, unknown> } | undefined;
    expect(saved?.values).toEqual({ project_default_enabled: true });
  });
});
