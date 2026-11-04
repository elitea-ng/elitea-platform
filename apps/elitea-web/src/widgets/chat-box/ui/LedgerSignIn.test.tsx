/**
 * FU-250: the fan-out sign-in card with the REAL `useMcpLogin`,
 * `useMcpAuthModal` and `McpAuthModal`. Only the network (MSW) and the socket
 * transport (the test client, which records emits) are stand-ins.
 *
 * The earlier card tests rendered it through a mocked slot, so they never ran
 * `useMcpLogin`: its fallback to the `test_mcp_connection` socket check, which
 * Main does not answer, made Authorize a silent no-op in a real browser.
 */
import type { ReactElement } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { LedgerSignInSlotProps } from '@/features/chat-messages';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient, type TestSocketClient } from '@/shared/api/socket/testing';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { LedgerSignIn } from './LedgerSignIn';

const CHALLENGE_PATH = '*/api/v2/elitea_core/mcp_authorization_challenge/prompt_lib/1/41';

const challenge = {
  server_url: 'https://mcp.example/mcp-auth',
  resource_metadata_url: 'https://mcp.example/.well-known/oauth-protected-resource/mcp-auth',
  resource_metadata: {
    resource: 'https://mcp.example/mcp-auth',
    authorization_servers: ['https://mcp.example'],
    oauth_authorization_server: {
      issuer: 'https://mcp.example',
      authorization_endpoint: 'https://mcp.example/authorize',
      token_endpoint: 'https://mcp.example/token',
      registration_endpoint: 'https://mcp.example/register',
      code_challenge_methods_supported: ['S256'],
      token_endpoint_auth_methods_supported: ['none'],
    },
  },
};

function slot(overrides: Partial<LedgerSignInSlotProps> = {}): LedgerSignInSlotProps {
  return {
    toolkitId: 41,
    toolkitType: 'mcp',
    serverUrl: 'https://card.example/mcp-auth',
    message: 'Authorization is required to use the fu250mcpauth MCP toolkit.',
    disabled: false,
    onAuthorized: vi.fn(),
    onSkip: vi.fn(),
    ...overrides,
  };
}

function render(ui: ReactElement, socket: TestSocketClient) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithTheme(
    <QueryClientProvider client={client}>
      <SocketClientContext.Provider value={socket}>{ui}</SocketClientContext.Provider>
    </QueryClientProvider>,
  );
}

describe('LedgerSignIn', () => {
  beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
  afterEach(() => resetGeneratedClient());

  it('Authorize asks Main for the challenge by toolkit id and opens the dialog with Authorize enabled', async () => {
    const requests: { url: string; body: string }[] = [];
    server.use(
      http.post(CHALLENGE_PATH, async ({ request }) => {
        requests.push({ url: request.url, body: await request.text() });
        return HttpResponse.json(challenge);
      }),
    );
    const socket = createTestSocketClient();
    render(<LedgerSignIn projectId="1" slot={slot()} />, socket);

    await userEvent.click(screen.getByRole('button', { name: 'Authorize' }));

    const dialog = await screen.findByRole('dialog');
    await waitFor(() => expect(within(dialog).getByRole('button', { name: 'Authorize' })).toBeEnabled());
    expect(requests).toHaveLength(1);
    // The card's own server URL never leaves the browser; only the toolkit id does.
    expect(requests[0]?.url).not.toContain('card.example');
    expect(requests[0]?.body).toBe('');
    expect(socket.getEmitted('test_mcp_connection')).toEqual([]);
  });

  it('a retry after a refusal clears the message and opens the dialog', async () => {
    let calls = 0;
    server.use(
      http.post(CHALLENGE_PATH, () => {
        calls += 1;
        return calls === 1 ? HttpResponse.json({ error: 'authorization_discovery_failed' }, { status: 502 }) : HttpResponse.json(challenge);
      }),
    );
    render(<LedgerSignIn projectId="1" slot={slot()} />, createTestSocketClient());

    await userEvent.click(screen.getByRole('button', { name: 'Authorize' }));
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Authorize' }));

    await screen.findByRole('dialog');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it.each([
    ['authorization_discovery_failed', 502, "The MCP server's sign-in details could not be read. Try again, or Skip."],
    ['authorization_discovery_timeout', 504, 'The MCP server did not answer in time. Try again, or Skip.'],
    ['toolkit_not_found', 404, 'This toolkit is not available to you, so it cannot be signed in to here.'],
  ])('a %s refusal shows a readable error, opens no dialog and keeps Skip', async (code, status, text) => {
    server.use(http.post(CHALLENGE_PATH, () => HttpResponse.json({ error: code }, { status })));
    const onSkip = vi.fn();
    render(<LedgerSignIn projectId="1" slot={slot({ onSkip })} />, createTestSocketClient());

    await userEvent.click(screen.getByRole('button', { name: 'Authorize' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(screen.queryByRole('dialog')).toBeNull();
    const skip = screen.getByRole('button', { name: 'Skip' });
    expect(skip).toBeEnabled();
    await userEvent.click(skip);
    expect(onSkip).toHaveBeenCalledTimes(1);
  });
});
