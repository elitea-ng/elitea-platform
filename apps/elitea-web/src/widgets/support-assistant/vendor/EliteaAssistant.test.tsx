/**
 * `EliteaAssistant` — the ported widget's root, end to end.
 *
 * NOTHING IS SUBSTITUTED but the network (`msw`) and the browser SSE global
 * (`installTestEventSource`): the real launcher, panel, header, message list,
 * input and the real `useChat`/`useSupportStream` wiring all run. That is the
 * only way a test can tell "the panel opened" from "the panel opened and threw
 * on mount" — the same rule `ui/SupportAssistantWidget.test.tsx` already
 * follows one level up.
 *
 * Covers: launcher, panel open/close, message send, a streamed answer through
 * every status step, the typewriter finishing and exposing Copy, three error
 * shapes, new chat, history switching, fullscreen, and the imperative popup.
 */
import { createRef } from 'react';

import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { EXECUTION_EVENT_FAILED, EXECUTION_EVENT_NODE } from '@/shared/api/sse/executionEvents';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { EliteaAssistant } from './EliteaAssistant';
import type { TAssistantConfig, TEliteaAssistantRef } from './lib/types';

const BASE = '/api/v2';

const CONFIG: TAssistantConfig = {
  enabled: true,
  title: 'ELITEA Support',
  welcome_message: 'Hi! Ask me anything about ELITEA.',
  placeholder: 'Type a message...',
  support_project_id: 7,
  user: { id: 1, name: 'Alice', avatar: '' },
};

let registry: TestEventSourceRegistry;

function useEmptyHistory(): void {
  server.use(
    http.get(`${BASE}/support_assistant/conversations/`, () => HttpResponse.json({ items: [], total: 0 })),
  );
}

function useHistoryOf(items: { uuid: string; name: string }[], conversations: Record<string, unknown>): void {
  server.use(
    http.get(`${BASE}/support_assistant/conversations/`, () =>
      HttpResponse.json({ items: items.map((item) => ({ ...conversationRow, ...item })), total: items.length }),
    ),
    http.get(`${BASE}/support_assistant/conversation/:id`, ({ params }) =>
      HttpResponse.json(conversations[String(params.id)] ?? { uuid: params.id, message_groups: [] }),
    ),
  );
}

const conversationRow = {
  id: 1,
  is_private: true,
  author_id: 1,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  meta: {
    is_hidden: false,
    context_strategy: {
      name: 'default',
      enabled: false,
      created_at: '2026-01-01T00:00:00Z',
      last_optimized_at: null,
      max_context_tokens: 0,
      enable_summarization: false,
      summary_instructions: '',
      summary_llm_settings: null,
      preserve_recent_messages: 0,
      preserve_system_messages: false,
    },
    conversation_type: 'support',
  },
  source: 'widget',
  attachment_participant_id: null,
  instructions: null,
  participants_count: 1,
  message_groups_count: 1,
  users_count: 1,
  duration: 0,
};

function renderAssistant(config: TAssistantConfig = CONFIG) {
  const ref = createRef<TEliteaAssistantRef>();
  const view = renderWithTheme(<EliteaAssistant ref={ref} config={config} />);
  return { ref, ...view };
}

/**
 * `StatusMessage` renders one <span> per character, and a SPACE renders as
 * U+00A0 (non-breaking) rather than U+0020 — paired with the `minWidth`
 * that component sets for the same character, that reads as a deliberate
 * guard against a lone space collapsing to zero width in a per-character
 * span. Matching it here rather than a plain space keeps the assertion
 * honest about what the DOM actually holds.
 */
function nbsp(text: string): string {
  return text.replace(/ /g, ' ');
}

async function openPanel(): Promise<void> {
  await userEvent.click(await screen.findByRole('button', { name: 'Support Assistant' }));
  await screen.findByRole('heading', { name: 'ELITEA Support' });
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  registry = installTestEventSource();
  useEmptyHistory();
  // jsdom implements no scroll layout at all; the message list scrolls itself
  // to the newest message on every render, which is otherwise a hard throw.
  Element.prototype.scrollIntoView = vi.fn();
});

afterEach(() => {
  resetGeneratedClient();
  registry.restore();
});

describe('EliteaAssistant — launcher and panel', () => {
  it('starts CLOSED, showing only the launcher button', () => {
    renderAssistant();
    expect(screen.getByRole('button', { name: 'Support Assistant' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'ELITEA Support' })).not.toBeInTheDocument();
  });

  it('opens the panel on a launcher click, showing the welcome message', async () => {
    renderAssistant();
    await openPanel();

    expect(screen.getByText('Hi! Ask me anything about ELITEA.')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Type a message...')).toBeInTheDocument();
  });

  it('closes the panel from its own header button, without removing the launcher', async () => {
    renderAssistant();
    await openPanel();

    await userEvent.click(screen.getByRole('button', { name: 'Close chat' }));

    expect(screen.queryByRole('heading', { name: 'ELITEA Support' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Support Assistant' })).toBeInTheDocument();
  });

  it('toggles open and closed through the imperative ref', async () => {
    const { ref } = renderAssistant();
    await waitFor(() => expect(ref.current).not.toBeNull());

    act(() => ref.current?.open());
    await screen.findByRole('heading', { name: 'ELITEA Support' });
    expect(ref.current?.isOpen()).toBe(true);

    act(() => ref.current?.close());
    await waitFor(() => {
      expect(screen.queryByRole('heading', { name: 'ELITEA Support' })).not.toBeInTheDocument();
    });
    expect(ref.current?.isOpen()).toBe(false);
  });
});

describe('EliteaAssistant — fullscreen', () => {
  it('expands and collapses via the header button and the overlay', async () => {
    renderAssistant();
    await openPanel();

    await userEvent.click(screen.getByRole('button', { name: 'Expand chat' }));
    expect(document.querySelector('.elitea-assistant-window--expanded')).not.toBeNull();
    const overlay = screen.getByRole('button', { name: 'Collapse the assistant' });

    await userEvent.click(overlay);
    expect(document.querySelector('.elitea-assistant-window--expanded')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Collapse the assistant' })).not.toBeInTheDocument();
  });

  it('expands, collapses and toggles fullscreen through the imperative ref', async () => {
    const { ref } = renderAssistant();
    await waitFor(() => expect(ref.current).not.toBeNull());
    act(() => ref.current?.open());
    await screen.findByRole('heading', { name: 'ELITEA Support' });

    act(() => ref.current?.expandFullscreen());
    expect(ref.current?.isExpanded()).toBe(true);

    act(() => ref.current?.collapseFullscreen());
    expect(ref.current?.isExpanded()).toBe(false);

    act(() => ref.current?.toggleFullscreen());
    expect(ref.current?.isExpanded()).toBe(true);
  });
});

describe('EliteaAssistant — the imperative popup', () => {
  it('shows and hides the proactive nudge without opening the panel', async () => {
    const { ref } = renderAssistant();
    await waitFor(() => expect(ref.current).not.toBeNull());

    act(() => ref.current?.showPopup());
    expect(await screen.findByText('Hi! Need help? Ask me!')).toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Close popup' }));
    await waitFor(() => {
      expect(screen.queryByText('Hi! Need help? Ask me!')).not.toBeInTheDocument();
    });
  });

  it('hides the popup the moment the panel is opened', async () => {
    const { ref } = renderAssistant();
    await waitFor(() => expect(ref.current).not.toBeNull());

    act(() => ref.current?.showPopup());
    await screen.findByText('Hi! Need help? Ask me!');

    act(() => ref.current?.open());

    await waitFor(() => {
      expect(screen.queryByText('Hi! Need help? Ask me!')).not.toBeInTheDocument();
    });
  });
});

describe('EliteaAssistant — sending a message and a streamed answer', () => {
  it('walks a question through every status step to a finished, copyable answer', async () => {
    server.use(
      http.post(`${BASE}/support_assistant/conversations/`, () =>
        HttpResponse.json({ ...conversationRow, uuid: 'c1', name: 'New chat' }),
      ),
      http.post(`${BASE}/support_assistant/predict/:id`, () =>
        HttpResponse.json({ events_url: '/stream/turn-1' }),
      ),
    );
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    });

    renderAssistant();
    await openPanel();

    const input = screen.getByPlaceholderText('Type a message...');
    await userEvent.type(input, 'How do I reset my password?{enter}');

    expect(screen.getByText('How do I reset my password?')).toBeInTheDocument();
    await waitFor(() => expect(registry.getSources().at(-1)?.url).toBe('/stream/turn-1'));

    registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' }));
    // `StatusMessage` renders one <span> per character, so the sentence is
    // matched by its assembled text content rather than by a single node.
    await waitFor(() => {
      expect(document.querySelector('.elitea-assistant-status-message')?.textContent).toBe(nbsp('Starting up...'));
    });
    // The input is disabled for the whole turn, not just while awaiting the
    // network call that started it.
    expect(input).toBeDisabled();

    registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'agent_llm_start', message_id: 'm1' }));
    await waitFor(() => {
      expect(document.querySelector('.elitea-assistant-status-message')?.textContent).toBe(
        nbsp('Looking things up...'),
      );
    });

    registry.emit(
      EXECUTION_EVENT_NODE,
      JSON.stringify({ type: 'chunk', message_id: 'm1', content: 'Open Settings' }),
    );
    await screen.findByText('Open Settings');

    registry.emit(
      EXECUTION_EVENT_NODE,
      JSON.stringify({ type: 'agent_response', message_id: 'm1', content: 'Open Settings and pick "Reset password".' }),
    );

    // The typewriter runs on a real interval; the finished text is the
    // observable end state, whatever cadence got there.
    await waitFor(
      () => {
        expect(screen.getByText('Open Settings and pick "Reset password".')).toBeInTheDocument();
      },
      { timeout: 3000 },
    );

    // The welcome message is a finished assistant message too, and carries
    // its own Copy button — the ANSWER's is the last one on screen.
    const copyButtons = await screen.findAllByRole('button', { name: 'Copy to clipboard' });
    await userEvent.click(copyButtons.at(-1) as HTMLElement);
    expect(writeText).toHaveBeenCalledWith('Open Settings and pick "Reset password".');

    await waitFor(() => expect(input).toBeEnabled());
  });

  it('does not send on Shift+Enter, and does not clear the draft', async () => {
    renderAssistant();
    await openPanel();

    const input = screen.getByPlaceholderText('Type a message...');
    await userEvent.type(input, 'still typing{shift>}{enter}{/shift}');

    expect(input).toHaveValue('still typing\n');
    // No message bubble — the draft stayed in the box, nothing was sent.
    expect(document.querySelector('.elitea-assistant-message--user')).toBeNull();
  });

  it('leaves the send button disabled for blank or whitespace-only input', async () => {
    renderAssistant();
    await openPanel();

    const sendButton = screen.getByRole('button', { name: 'Send message' });
    expect(sendButton).toBeDisabled();

    await userEvent.type(screen.getByPlaceholderText('Type a message...'), '   ');
    expect(sendButton).toBeDisabled();
  });
});

describe('EliteaAssistant — error states', () => {
  beforeEach(() => {
    server.use(
      http.post(`${BASE}/support_assistant/conversations/`, () =>
        HttpResponse.json({ ...conversationRow, uuid: 'c1', name: 'New chat' }),
      ),
    );
  });

  it('reports a failed predict call as an assistant error message', async () => {
    server.use(
      http.post(`${BASE}/support_assistant/predict/:id`, () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    renderAssistant();
    await openPanel();

    await userEvent.type(screen.getByPlaceholderText('Type a message...'), 'Hello?{enter}');

    expect(
      await screen.findByText('Failed to reach the support assistant. Please try again.'),
    ).toBeInTheDocument();
  });

  it('reports a 200 with no events_url instead of spinning forever', async () => {
    server.use(http.post(`${BASE}/support_assistant/predict/:id`, () => HttpResponse.json({})));
    renderAssistant();
    await openPanel();

    await userEvent.type(screen.getByPlaceholderText('Type a message...'), 'Hello?{enter}');

    expect(
      await screen.findByText('The support assistant did not return a response stream.'),
    ).toBeInTheDocument();
  });

  it('turns a server-reported stream failure into the shown error', async () => {
    server.use(
      http.post(`${BASE}/support_assistant/predict/:id`, () =>
        HttpResponse.json({ events_url: '/stream/turn-1' }),
      ),
    );
    renderAssistant();
    await openPanel();

    await userEvent.type(screen.getByPlaceholderText('Type a message...'), 'Hello?{enter}');
    await waitFor(() => expect(registry.getSources().at(-1)?.url).toBe('/stream/turn-1'));

    registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' }));
    registry.emit(EXECUTION_EVENT_FAILED, JSON.stringify({ safe_message: 'The support agent is unavailable.' }));

    expect(await screen.findByText('The support agent is unavailable.')).toBeInTheDocument();
  });
});

describe('EliteaAssistant — history and new chat', () => {
  it('opens on the most recent conversation\'s transcript, not the welcome message', async () => {
    useHistoryOf(
      [{ uuid: 'c1', name: 'Password reset' }],
      { c1: { uuid: 'c1', message_groups: [{ uuid: 'g1', sent_to: 5, message_items: [{ item_type: 'text_message', content: 'Earlier question' }] }] } },
    );
    renderAssistant();
    await openPanel();

    expect(await screen.findByText('Earlier question')).toBeInTheDocument();
    expect(screen.queryByText('Hi! Ask me anything about ELITEA.')).not.toBeInTheDocument();
  });

  it('switches to a different conversation from the history dropdown', async () => {
    useHistoryOf(
      [
        { uuid: 'c1', name: 'Password reset' },
        { uuid: 'c2', name: 'Billing question' },
      ],
      {
        c1: { uuid: 'c1', message_groups: [{ uuid: 'g1', sent_to: 5, message_items: [{ item_type: 'text_message', content: 'About passwords' }] }] },
        c2: { uuid: 'c2', message_groups: [{ uuid: 'g2', sent_to: 5, message_items: [{ item_type: 'text_message', content: 'About billing' }] }] },
      },
    );
    renderAssistant();
    await openPanel();
    await screen.findByText('About passwords');

    await userEvent.click(screen.getByRole('button', { name: 'Chat history' }));
    await userEvent.click(screen.getByRole('button', { name: 'Billing question' }));

    expect(await screen.findByText('About billing')).toBeInTheDocument();
    expect(screen.queryByText('About passwords')).not.toBeInTheDocument();
  });

  it('starts a NEW chat, dropping the current transcript', async () => {
    useHistoryOf(
      [{ uuid: 'c1', name: 'Password reset' }],
      { c1: { uuid: 'c1', message_groups: [{ uuid: 'g1', sent_to: 5, message_items: [{ item_type: 'text_message', content: 'Earlier question' }] }] } },
    );
    renderAssistant();
    await openPanel();
    await screen.findByText('Earlier question');

    await userEvent.click(screen.getByRole('button', { name: 'New chat' }));

    expect(await screen.findByText('Hi! Ask me anything about ELITEA.')).toBeInTheDocument();
    expect(screen.queryByText('Earlier question')).not.toBeInTheDocument();
  });

  it('disables the history button when there is no history to show', async () => {
    renderAssistant();
    await openPanel();

    expect(screen.getByRole('button', { name: 'Chat history' })).toBeDisabled();
  });
});

describe('EliteaAssistant — the history dropdown closes on an outside click', () => {
  it('closes the dropdown when the user clicks elsewhere', async () => {
    useHistoryOf([{ uuid: 'c1', name: 'Password reset' }], {});
    renderAssistant();
    await openPanel();
    await screen.findByRole('button', { name: 'Chat history' });

    await userEvent.click(screen.getByRole('button', { name: 'Chat history' }));
    const dropdownItem = await screen.findByRole('button', { name: 'Password reset' });
    expect(
      within(dropdownItem.closest('.elitea-assistant-history-dropdown') as HTMLElement).getByText('Password reset'),
    ).toBeInTheDocument();

    await userEvent.click(screen.getByPlaceholderText('Type a message...'));

    await waitFor(() => {
      expect(screen.queryByText('Password reset')).not.toBeInTheDocument();
    });
  });
});

describe('EliteaAssistant — tooltips', () => {
  it('shows a tooltip on hover over an action button', async () => {
    renderAssistant();
    await openPanel();

    await userEvent.hover(screen.getByRole('button', { name: 'New chat' }));
    expect(await screen.findByRole('tooltip')).toHaveTextContent('New conversation');

    await userEvent.unhover(screen.getByRole('button', { name: 'New chat' }));
    await waitFor(() => {
      expect(screen.queryByRole('tooltip')).not.toBeInTheDocument();
    });
  });
});
