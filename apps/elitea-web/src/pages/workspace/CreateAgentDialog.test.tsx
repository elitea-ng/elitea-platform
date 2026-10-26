import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, screen } from '@testing-library/react';
import type { ReactElement } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { CreateAgentDialog, type CreateAgentDialogProps } from './CreateAgentDialog';

function renderDialog(overrides: Partial<CreateAgentDialogProps> = {}): CreateAgentDialogProps {
  const props: CreateAgentDialogProps = {
    open: true,
    onClose: vi.fn(),
    projectId: 42,
    localFolder: true,
    canSendLocal: true,
    onLocal: vi.fn(),
    onCreated: vi.fn(),
    ...overrides,
  };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme((<QueryClientProvider client={queryClient}><CreateAgentDialog {...props} /></QueryClientProvider>) as ReactElement);
  return props;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('CreateAgentDialog', () => {
  it('asks the one question in a local folder session', () => {
    renderDialog();
    expect(screen.getByText('Which kind of agent?')).toBeInTheDocument();
    expect(screen.getByText('Local agent')).toBeInTheDocument();
    expect(screen.getByText('Elitea agent')).toBeInTheDocument();
  });

  it('sends the local agent request into the thread, for a nested folder', () => {
    const props = renderDialog();
    fireEvent.click(screen.getByText('Local agent'));
    fireEvent.change(screen.getByLabelText('What should the agent do?'), { target: { value: 'Review React code' } });
    fireEvent.change(screen.getByLabelText('Folder'), { target: { value: 'apps/web' } });
    fireEvent.click(screen.getByText('Ask the agent to write it'));

    expect(props.onLocal).toHaveBeenCalledTimes(1);
    const prompt = vi.mocked(props.onLocal).mock.calls[0]?.[0] ?? '';
    expect(prompt).toContain('`apps/web/AGENTS.md`');
    expect(prompt).toContain('Review React code');
    expect(props.onClose).toHaveBeenCalled();
  });

  it('refuses a folder outside the workspace and a thread that cannot send', () => {
    renderDialog({ canSendLocal: false });
    fireEvent.click(screen.getByText('Local agent'));
    fireEvent.change(screen.getByLabelText('What should the agent do?'), { target: { value: 'x' } });
    expect(screen.getByText('Ask the agent to write it').closest('button')).toBeDisabled();
    expect(screen.getByText(/Pick an agent for this thread/)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText('Folder'), { target: { value: '../elsewhere' } });
    expect(screen.getByText(/Use a folder inside this one/)).toBeInTheDocument();
  });

  it('goes straight to the Elitea builder in a thread that runs in the cloud', () => {
    renderDialog({ localFolder: false });
    expect(screen.queryByText('Which kind of agent?')).toBeNull();
    expect(screen.getByText('Build with AI')).toBeInTheDocument();
  });
});
