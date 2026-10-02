import { fireEvent, screen } from '@testing-library/react';
import { beforeEach, expect, it, vi } from 'vitest';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { mocks } from './__mocks__/ToolkitTestAuthorization.mock';
import { ToolkitTestAuthorization } from './ToolkitTestAuthorization';

const challenge = { toolkit_id: 9, toolkit_name: 'Docs', toolkit_type: 'mcp', server_url: 'https://mcp.example/tools', resource_metadata: {} };
beforeEach(() => vi.clearAllMocks());

it('uses the existing OAuth action and retries only with its scoped saved reference', () => {
  mocks.reference.mockReturnValue('R'.repeat(43));
  const onAuthorized = vi.fn(() => Promise.resolve(undefined));
  renderWithTheme(<ToolkitTestAuthorization projectId="7" challenge={challenge} onAuthorized={onAuthorized} onSkip={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Authorize' }));
  expect(mocks.login).toHaveBeenCalledOnce();
  fireEvent.click(screen.getByText('Finish OAuth'));
  expect(mocks.reference).toHaveBeenCalledWith('7', '9', challenge.server_url);
  expect(onAuthorized).toHaveBeenCalledWith('R'.repeat(43));
});

it('does not retry when OAuth has no matching saved reference and supports local Skip', () => {
  mocks.reference.mockReturnValue(undefined);
  const onAuthorized = vi.fn(() => Promise.resolve(undefined));
  const onSkip = vi.fn();
  renderWithTheme(<ToolkitTestAuthorization projectId="7" challenge={challenge} onAuthorized={onAuthorized} onSkip={onSkip} />);
  fireEvent.click(screen.getByText('Finish OAuth'));
  expect(onAuthorized).not.toHaveBeenCalled();
  expect(screen.getByRole('alert')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Skip' }));
  expect(onSkip).toHaveBeenCalledOnce();
});
