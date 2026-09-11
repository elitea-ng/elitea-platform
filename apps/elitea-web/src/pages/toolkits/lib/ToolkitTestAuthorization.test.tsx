import { fireEvent, screen } from '@testing-library/react';
import { beforeEach, expect, it, vi } from 'vitest';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { ToolkitTestAuthorization } from './ToolkitTestAuthorization';

const mocks = vi.hoisted(() => ({ reference: vi.fn(), login: vi.fn() }));
vi.mock('@/features/mcps', () => ({
  getAuthorizationReference: mocks.reference,
  useMcpLogin: (options: { onSuccess: () => void }) => ({ onLogin: mocks.login, modalProps: { onClose: () => options.onSuccess(), onCancel: vi.fn() } }),
  McpAuthModal: ({ onClose, onCancel }: { onClose: () => void; onCancel: () => void }) => <><button onClick={onClose}>Finish OAuth</button><button onClick={onCancel}>Cancel OAuth</button></>,
}));
const challenge = { toolkit_id: 9, toolkit_name: 'Docs', toolkit_type: 'mcp', server_url: 'https://mcp.example/tools', resource_metadata: {} };
beforeEach(() => vi.clearAllMocks());

it('uses the existing OAuth action and retries only with its scoped saved reference', () => {
  mocks.reference.mockReturnValue('R'.repeat(43));
  const onAuthorized = vi.fn(async () => undefined);
  renderWithTheme(<ToolkitTestAuthorization projectId="7" challenge={challenge} onAuthorized={onAuthorized} onSkip={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Authorize' }));
  expect(mocks.login).toHaveBeenCalledOnce();
  fireEvent.click(screen.getByText('Finish OAuth'));
  expect(mocks.reference).toHaveBeenCalledWith('7', '9', challenge.server_url);
  expect(onAuthorized).toHaveBeenCalledWith('R'.repeat(43));
});

it('does not retry when OAuth has no matching saved reference and supports local Skip', () => {
  mocks.reference.mockReturnValue(undefined);
  const onAuthorized = vi.fn(async () => undefined);
  const onSkip = vi.fn();
  renderWithTheme(<ToolkitTestAuthorization projectId="7" challenge={challenge} onAuthorized={onAuthorized} onSkip={onSkip} />);
  fireEvent.click(screen.getByText('Finish OAuth'));
  expect(onAuthorized).not.toHaveBeenCalled();
  expect(screen.getByRole('alert')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Skip' }));
  expect(onSkip).toHaveBeenCalledOnce();
});
