/**
 * The `managed_surface` dispatch on `pages/admin/Configuration.tsx`.
 *
 * A section can now be BOTH "unavailable" (the plugin-config value endpoints
 * cannot serve it) and editable (a dedicated surface holds its data). That pair
 * is unusual enough to be worth its own file, and each case here guards one way
 * the dispatch could be wrong in a way nothing else would notice:
 *
 *  1. A managed section renders its EDITOR, not its refusal — otherwise the
 *     port lands with a working backend nobody can reach.
 *  2. It is not labelled "Not available here" in the sidebar, which would send
 *     an operator away from the only page that can edit it.
 *  3. An UNRECOGNISED `managed_surface` falls back to the refusal rather than to
 *     a blank pane. This is the case that matters on an older client against a
 *     newer server, and it is the reason `unavailable_reason` is still sent for
 *     a managed section.
 *  4. The plugin-config value endpoint is still NOT called for it. The section
 *     is managed elsewhere; a values fetch would spend a request to be told 501.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen } from '@testing-library/react';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminConfiguration } from './Configuration';
import { renderAdminRoute } from './__tests__/testRouter';

const WITHHELD_REASON = 'these settings have no equivalent on this platform';

const MCP_REASON =
  'MCP server definitions are not stored as plugin configuration on this platform';

let recorded: string[] = [];

/** The sections the server declares, with the MCP one carrying BOTH keys. */
function sections(managedSurface: string | undefined) {
  return [
    {
      id: 'guardrails',
      title: 'Guardrails',
      unavailable_reason: WITHHELD_REASON,
      fields: [],
    },
    {
      id: 'mcp_servers',
      title: 'MCP Servers',
      // Both, deliberately: the value endpoints still cannot serve it.
      ...(managedSurface === undefined ? {} : { managed_surface: managedSurface }),
      unavailable_reason: MCP_REASON,
      fields: [],
    },
  ];
}

function useHandlers(managedSurface: string | undefined): void {
  server.use(
    http.get('*/admin/plugin_config_schemas/administration', () =>
      HttpResponse.json({ sections: sections(managedSurface) }),
    ),
    http.get('*/admin/plugin_config_values/administration/:section', ({ params }) => {
      recorded.push(String(params['section']));
      return HttpResponse.json({ error: 'unavailable' }, { status: 501 });
    }),
    http.get('*/admin/mcp_prebuilt_servers/administration', () =>
      HttpResponse.json({ servers: [], total: 0 }),
    ),
  );
}

beforeEach(() => {
  recorded = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AdminConfiguration — a server-managed section', () => {
  it('renders the dedicated editor instead of the refusal', async () => {
    useHandlers('mcp_prebuilt_servers');
    renderAdminRoute(<AdminConfiguration />);

    // The catalogue editor's own empty state, so this asserts the EDITOR
    // mounted and fetched — not merely that the refusal is absent.
    expect(await screen.findByTestId('admin-mcp-servers-empty')).toBeInTheDocument();
    expect(screen.queryByTestId('admin-configuration-unavailable')).not.toBeInTheDocument();
    expect(screen.getByTestId('admin-mcp-servers-add')).toBeInTheDocument();
  });

  it('does not label a managed section "Not available here"', async () => {
    useHandlers('mcp_prebuilt_servers');
    renderAdminRoute(<AdminConfiguration />);
    await screen.findByTestId('admin-mcp-servers-empty');

    // Guardrails is genuinely unavailable and keeps the label; MCP Servers does
    // not. One label present, not two.
    expect(screen.getAllByText('Not available here')).toHaveLength(1);
  });

  it('does not fetch plugin-config values for it', async () => {
    useHandlers('mcp_prebuilt_servers');
    renderAdminRoute(<AdminConfiguration />);
    await screen.findByTestId('admin-mcp-servers-empty');

    expect(recorded).not.toContain('mcp_servers');
  });

  it('falls back to the server reason when the surface is not one this build renders', async () => {
    // An older client against a newer server: the section names a surface this
    // build has no editor for. It must explain itself, not render nothing.
    useHandlers('some_future_surface');
    renderAdminRoute(<AdminConfiguration />);

    expect(await screen.findByTestId('admin-configuration-unavailable')).toHaveTextContent(
      MCP_REASON,
    );
    expect(screen.queryByTestId('admin-mcp-servers-add')).not.toBeInTheDocument();
  });

  it('renders the refusal when the server declares no managed surface at all', async () => {
    useHandlers(undefined);
    renderAdminRoute(<AdminConfiguration />);

    expect(await screen.findByTestId('admin-configuration-unavailable')).toBeInTheDocument();
    expect(screen.queryByTestId('admin-mcp-servers-add')).not.toBeInTheDocument();
  });
});
