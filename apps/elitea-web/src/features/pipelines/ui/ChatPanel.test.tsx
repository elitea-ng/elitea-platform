import { createRef } from 'react';

import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { ThemeProvider } from '@mui/material/styles';

import { usePipelineYamlStore } from '../model/pipelineYamlStore';
import { ChatPanel } from './ChatPanel';
import type { ChatPanelHandle, ChatPanelProps } from './ChatPanel';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

beforeEach(() => {
  Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 1280 });
  usePipelineYamlStore.setState({
    yamlCode: 'a: 1',
    yamlJsonObject: { a: 1 },
    initYamlCode: 'a: 1',
    initYamlJsonObject: { a: 1 },
    resetFlag: false,
    layoutVersion: undefined,
  });
});

afterEach(() => {
  Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 1024 });
});

function renderChatPanel(props: Partial<ChatPanelProps> = {}, path = '/pipelines/latest/1', ref?: React.Ref<ChatPanelHandle>) {
  const rootRoute = createRootRoute({
    component: () => (
      <ThemeProvider
        theme={theme}
        defaultMode={DEFAULT_COLOR_SCHEME}
      >
        <ChatPanel
          settings={{}}
          setActiveConversation={vi.fn()}
          renderChat={() => <div data-testid="chat-slot" />}
          ref={ref}
          {...props}
        />
      </ThemeProvider>
    ),
  });
  const router = createRouter({ routeTree: rootRoute, history: createMemoryHistory({ initialEntries: [path] }) });
  return render(<RouterProvider router={router} />);
}

describe('ChatPanel', () => {
  it('renders the chat slot by default (not collapsed)', async () => {
    renderChatPanel();
    expect(await screen.findByTestId('chat-slot')).toBeInTheDocument();
  });

  it('collapsing hides the chat slot and the top bar', async () => {
    const user = userEvent.setup();
    renderChatPanel();

    await screen.findByTestId('chat-slot');
    await user.click(screen.getAllByRole('button')[0] as HTMLElement);

    expect(screen.queryByTestId('chat-slot')).not.toBeInTheDocument();
  });

  it('calls onCollapsed with the new collapsed state', async () => {
    const user = userEvent.setup();
    const onCollapsed = vi.fn();
    renderChatPanel({ onCollapsed });

    await screen.findByTestId('chat-slot');
    await user.click(screen.getAllByRole('button')[0] as HTMLElement);
    expect(onCollapsed).toHaveBeenCalledWith(true);
  });

  // #135: icon-only collapse button with no accessible name — the second of
  // the two critical `button-name` violations on the pipeline editor screen.
  it('names the collapse toggle for a screen reader, and the name follows the state', async () => {
    const user = userEvent.setup();
    renderChatPanel();

    await screen.findByTestId('chat-slot');
    const button = screen.getByRole('button', { name: 'Collapse the chat panel' });
    await user.click(button);

    expect(screen.getByRole('button', { name: 'Expand the chat panel' })).toBeInTheDocument();
  });

  it('renders ViewRunHistoryButton only when onShowHistory is given', async () => {
    const { rerender: _unused } = renderChatPanel({ onShowHistory: undefined });
    await screen.findByTestId('chat-slot');
    expect(screen.queryByTestId('pipeline-history-tab')).not.toBeInTheDocument();
  });

  it('renders ViewRunHistoryButton when onShowHistory is provided, and clicking it calls the handler', async () => {
    const user = userEvent.setup();
    const onShowHistory = vi.fn();
    renderChatPanel({ onShowHistory });

    const historyButton = await screen.findByTestId('pipeline-history-tab');
    await user.click(historyButton);
    expect(onShowHistory).toHaveBeenCalled();
  });

  it('renders the renderClearChatButton slot with a computed disabled flag (true when chat_history is empty)', async () => {
    const renderClearChatButton = vi.fn<NonNullable<ChatPanelProps['renderClearChatButton']>>(() => <button type="button">clear</button>);
    renderChatPanel({
      renderClearChatButton,
      settings: { activeConversation: { chat_history: [] } },
    });
    await screen.findByTestId('chat-slot');
    const call = renderClearChatButton.mock.calls[0]?.[0];
    expect(call?.disabled).toBe(true);
    expect(typeof call?.onClear).toBe('function');
  });

  it('renderClearChatButton disabled=false when there is real, non-welcome chat history and not streaming', async () => {
    const renderClearChatButton = vi.fn<NonNullable<ChatPanelProps['renderClearChatButton']>>(() => <button type="button">clear</button>);
    renderChatPanel({
      renderClearChatButton,
      settings: {
        activeConversation: { chat_history: [{ id: 'm1', role: 'user' }, { id: 'm2', role: 'assistant' }] },
        isStreaming: false,
      },
    });
    await screen.findByTestId('chat-slot');
    const call = renderClearChatButton.mock.calls[0]?.[0];
    expect(call?.disabled).toBe(false);
    expect(typeof call?.onClear).toBe('function');
  });

  it('renders the renderContextBudget slot only once activeConversation has a real id', async () => {
    const renderContextBudget = vi.fn(() => <div data-testid="budget-slot" />);
    renderChatPanel({ renderContextBudget, settings: { activeConversation: { chat_history: [] } } });
    await screen.findByTestId('chat-slot');
    expect(screen.queryByTestId('budget-slot')).not.toBeInTheDocument();

    renderContextBudget.mockClear();
  });

  it('renders the renderContextBudget slot with the conversation id/context/instructions once id is set', async () => {
    const renderContextBudget = vi.fn(() => <div data-testid="budget-slot" />);
    const setActiveConversation = vi.fn();
    renderChatPanel({
      renderContextBudget,
      setActiveConversation,
      settings: {
        activeConversation: {
          id: 7,
          chat_history: [],
          meta: { context_strategy: { window: 10 } },
          instructions: 'be nice',
        },
      },
    });
    await screen.findByTestId('budget-slot');
    expect(renderContextBudget).toHaveBeenCalledWith({
      conversationId: 7,
      contextStrategy: { window: 10 },
      setActiveConversation,
      conversationInstructions: 'be nice',
    });
  });

  it('onClear stops any in-progress run before clearing when hasRunsInProgress reports true (baseline `ChatPanel.jsx`\'s onClear safety check)', async () => {
    const renderClearChatButton = vi.fn<NonNullable<ChatPanelProps['renderClearChatButton']>>(() => <button type="button">clear</button>);
    const stopAll = vi.fn();
    const onClearSlot = vi.fn();
    renderChatPanel({
      renderClearChatButton,
      hasRunsInProgress: () => true,
      renderChat: (slotProps) => {
        if (slotProps.ref && typeof slotProps.ref === 'object' && 'current' in slotProps.ref) {
          slotProps.ref.current = { stopAll, onClear: onClearSlot };
        }
        return <div data-testid="chat-slot" />;
      },
      settings: { activeConversation: { chat_history: [{ id: 'm1', role: 'user' }] } },
    });
    await screen.findByTestId('chat-slot');

    const onClear = renderClearChatButton.mock.calls[0]?.[0]?.onClear;
    onClear?.();

    expect(stopAll).toHaveBeenCalled();
    expect(onClearSlot).toHaveBeenCalled();
  });

  it('onClear does NOT stop any run when hasRunsInProgress reports false (or is not provided)', async () => {
    const renderClearChatButton = vi.fn<NonNullable<ChatPanelProps['renderClearChatButton']>>(() => <button type="button">clear</button>);
    const stopAll = vi.fn();
    const onClearSlot = vi.fn();
    renderChatPanel({
      renderClearChatButton,
      hasRunsInProgress: () => false,
      renderChat: (slotProps) => {
        if (slotProps.ref && typeof slotProps.ref === 'object' && 'current' in slotProps.ref) {
          slotProps.ref.current = { stopAll, onClear: onClearSlot };
        }
        return <div data-testid="chat-slot" />;
      },
      settings: { activeConversation: { chat_history: [{ id: 'm1', role: 'user' }] } },
    });
    await screen.findByTestId('chat-slot');

    const onClear = renderClearChatButton.mock.calls[0]?.[0]?.onClear;
    onClear?.();

    expect(stopAll).not.toHaveBeenCalled();
    expect(onClearSlot).toHaveBeenCalled();
  });

  it('exposes stopRun on the imperative handle, forwarding to the chat slot ref stopAll', async () => {
    const ref = createRef<ChatPanelHandle>();
    const stopAll = vi.fn();
    const onClear = vi.fn();
    renderChatPanel(
      {
        renderChat: (slotProps) => {
          // Simulate the future real chat component attaching to the injected ref.
          if (slotProps.ref && typeof slotProps.ref === 'object' && 'current' in slotProps.ref) {
            slotProps.ref.current = { stopAll, onClear };
          }
          return <div data-testid="chat-slot" />;
        },
      },
      '/pipelines/latest/1',
      ref,
    );

    await screen.findByTestId('chat-slot');
    ref.current?.stopRun();
    expect(stopAll).toHaveBeenCalled();
  });

  it('uses auto height and visible overflow on narrow viewports (< 1200px) so stacked panels never overlap', async () => {
    Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 900 });
    renderChatPanel();
    const content = await screen.findByTestId('pipeline-chat-content');
    const container = content.parentElement as HTMLElement;
    expect(getComputedStyle(container).minWidth).toBe('100%');
    expect(getComputedStyle(container).height).toBe('auto');
    expect(getComputedStyle(content).overflowY).toBe('visible');
  });

  it('supports on-demand resizable panel mode: stays mounted when closed, renders resize handle and close button when open', async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    const { unmount } = renderChatPanel({ panel: { isOpen: false, onClose } });

    const closedPanel = await screen.findByTestId('pipeline-test-panel');
    expect(closedPanel).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getByTestId('chat-slot')).toBeInTheDocument();
    unmount();

    renderChatPanel({ panel: { isOpen: true, onClose } });
    expect(await screen.findByTestId('pipeline-test-panel-resize-handle')).toBeInTheDocument();
    await user.click(screen.getByTestId('pipeline-test-panel-close'));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
