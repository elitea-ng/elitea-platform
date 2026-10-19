/** Visual parity with the baseline's `ChatContinue.jsx`, plus the slot/gating behaviour this app keeps. */
import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen } from '@testing-library/react';
import type { ReactNode } from 'react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { ChatContinue } from './ChatContinue';
import type { McpAuthRequiredAction } from './ChatContinue';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
const wrap = (node: ReactNode) => render(<ThemeProvider theme={theme}>{node}</ThemeProvider>);

const ACTION = {
  name: 'list_items',
  authorizationRequestId: 'auth-1',
  toolMeta: {
    toolkit_name: 'github',
    server_url: 'https://mcp.example',
    resource_metadata_url: 'https://mcp.example/.well-known/oauth-protected-resource',
    authorization_servers: ['https://login.example'],
    scopes: ['repo'],
  },
  toolOutputs: { server_url: 'https://mcp.example', authorization_servers: ['https://login.example'] },
} as McpAuthRequiredAction;

describe('ChatContinue — toolkit authorization panel', () => {
  it('renders the baseline panel, message, details and both buttons', () => {
    const onSkip = vi.fn();
    wrap(<ChatContinue authRequired authRequiredAction={ACTION} onContinueWithoutAuth={onSkip} continueLabel="Skip" renderAuthModal={() => null} />);

    const panel = screen.getByTestId('toolkit-authorization-panel');
    expect(panel.getAttribute('data-authorization-request-id')).toBe('auth-1');
    expect(panel.textContent).toContain('⚠️ Toolkit Authorization Required');
    expect(panel.textContent).toContain(
      'Authorization is required for toolkit "github". Authorize now or skip it for this run?',
    );

    const header = screen.getByRole('button', { name: /Authorization details ▸/ });
    expect(header.getAttribute('aria-expanded')).toBe('false');
    fireEvent.click(header);
    expect(screen.getByRole('button', { name: /Authorization details ▾/ })).toBeTruthy();
    expect(panel.textContent).toContain('Toolkit server:https://mcp.example');
    expect(panel.textContent).toContain('Authorization server:https://login.example');
    expect(panel.textContent).toContain('Scopes:repo');

    expect(screen.getByTestId('toolkit-authorization-authorize-button').textContent).toBe('Authorize');
    const skip = screen.getByTestId('toolkit-authorization-skip-button');
    expect(skip.textContent).toBe('Skip');
    fireEvent.click(skip);
    expect(onSkip).toHaveBeenCalledTimes(1);
  });

  it('labels the second button Continue by default', () => {
    wrap(<ChatContinue authRequired authRequiredAction={ACTION} />);
    expect(screen.getByTestId('toolkit-authorization-skip-button').textContent).toBe('Continue');
  });

  it('opens the slot on Authorize and reports success only from a real login', () => {
    const onAuthSuccess = vi.fn();
    wrap(
      <ChatContinue
        authRequired
        authRequiredAction={ACTION}
        onAuthSuccess={onAuthSuccess}
        renderAuthModal={(props) => <button onClick={() => props.onClose(true)}>Complete OAuth</button>}
      />,
    );
    fireEvent.click(screen.getByTestId('toolkit-authorization-authorize-button'));
    expect(onAuthSuccess).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Complete OAuth' }));
    expect(onAuthSuccess).toHaveBeenCalledTimes(1);
  });

  it('disables Authorize and says so when the authorization details are unavailable', () => {
    wrap(<ChatContinue authRequired authRequiredAction={{ name: 'x' }} renderAuthModal={() => null} />);
    expect(screen.getByTestId('toolkit-authorization-authorize-button').hasAttribute('disabled')).toBe(true);
    expect(screen.getByText('Authorization details are unavailable. You can skip this tool.')).toBeTruthy();
  });
});

describe('ChatContinue — confirmation panel', () => {
  it('renders the message and a Continue button', () => {
    const onContinue = vi.fn();
    wrap(<ChatContinue requiresConfirmation message="Paused before the next step." onContinue={onContinue} />);
    expect(screen.getByText('Paused before the next step.')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(onContinue).toHaveBeenCalledTimes(1);
  });

  it('renders nothing when neither auth nor confirmation is required', () => {
    const { container } = wrap(<ChatContinue />);
    expect(container.textContent).toBe('');
  });
});
