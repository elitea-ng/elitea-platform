/**
 * The desktop composer row: what `buildChatBoxInputSlots` hands the footer
 * when `native` is on, and the ⋯ menu that keeps every control the web row
 * shows as a button (model settings, read aloud, voice settings, clear).
 */
import type { ReactElement } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { PlusChatButton, SendButton, VoiceButton } from '@/widgets/chat';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

import { ChatBoxComposerTools, type ChatBoxComposerToolsProps } from './ChatBoxComposerTools';
import { buildChatBoxInputSlots } from './ChatBoxInputSlots';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function voiceProps(isPlaying = false) {
  return {
    isPlaying,
    onPlay: vi.fn(),
    onStop: vi.fn(),
    voiceConfig: { voiceName: null, voiceId: null, rate: 1, volume: 1 },
    voices: [],
    onVoiceConfigChange: vi.fn(),
    ttsModel: null,
    hasModelTTS: false,
    projectId: '42',
  };
}

const MODELS = [
  { id: 'a', name: 'openai/gpt-5-mini', shared: true },
  { id: 'b', name: 'qwen3.8-27b' },
];

function toolsProps(overrides: Partial<ChatBoxComposerToolsProps> = {}): ChatBoxComposerToolsProps {
  return {
    model: { shown: true, models: MODELS, selectedModel: MODELS[0], onSelectModel: vi.fn(), llmSettings: {}, onSetLLMSettings: vi.fn() },
    clearChat: { disabled: false, onClear: vi.fn() },
    voice: voiceProps(),
    ...overrides,
  };
}

function renderTools(props: ChatBoxComposerToolsProps): void {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <ChatBoxComposerTools {...props} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

async function openMore(): Promise<void> {
  fireEvent.click(screen.getByTestId('composer-more'));
  await screen.findByRole('menu');
}

beforeEach(() => {
  server.use(
    http.get('*/elitea_core/platform_settings/prompt_lib', () =>
      HttpResponse.json({ voice_features_enabled: true, voice_features_temporarily_disabled: false }),
    ),
  );
});

function nativeSlots(isAgentsPage: boolean) {
  return buildChatBoxInputSlots({
    attachments: { attachments: [], onAttachFiles: vi.fn(), disabled: false },
    internalTools: { disabled: false, tools: [], onToolChange: vi.fn() },
    model: { llmSettings: undefined, onSetLLMSettings: undefined, selectedModel: undefined, onSelectModel: undefined, models: [], shown: false },
    clearChat: { disabled: false, onClear: vi.fn() },
    refs: { attachmentButtonRef: { current: null }, voiceButtonRef: { current: null }, voiceInputRef: { current: null } },
    voice: voiceProps(),
    voiceInput: { onRecordingChange: vi.fn(), onError: vi.fn() },
    isAgentsPage,
    entitySubmenus: undefined,
    participants: undefined,
    native: true,
  });
}

const element = (node: unknown): ReactElement<Record<string, unknown>> => node as ReactElement<Record<string, unknown>>;

describe('buildChatBoxInputSlots — the desktop row', () => {
  it('leads with "+", the model and ⋯, and ends with the mic and the round send', () => {
    const slots = nativeSlots(false);
    expect(element(slots.attachmentButton).type).toBe(PlusChatButton);
    expect(element(slots.attachmentButton).props['native']).toBe(true);
    expect(element(slots.clearChat).type).toBe(ChatBoxComposerTools);
    expect((element(slots.clearChat).props['model'] as { shown: boolean }).shown).toBe(false);
    expect(slots.modelSelector).toBeUndefined();
    expect(element(slots.voiceButton).type).toBe(VoiceButton);
    expect(element(slots.voiceButton).props['native']).toBe(true);
    const send = element(slots.sendControl({ question: '', disabledSend: false, onSend: vi.fn(), isSpeakingMode: false, isRecording: false } as never));
    expect(send.type).toBe(SendButton);
    expect(send.props['native']).toBe(true);
  });

  it('keeps the web controls on the agents page', () => {
    const slots = nativeSlots(true);
    expect(element(slots.attachmentButton).props['native']).toBeUndefined();
    expect(slots.clearChat).toBeUndefined();
    expect(slots.modelSelector).toBeDefined();
  });
});

describe('ChatBoxComposerTools', () => {
  it('shows the short model name and picks from the list', async () => {
    const props = toolsProps();
    renderTools(props);
    fireEvent.click(screen.getByTestId('model-selector-name'));
    expect(screen.getByTestId('model-selector-name')).toHaveTextContent('gpt-5-mini');
    fireEvent.click(await screen.findByRole('menuitemradio', { name: /qwen3.8-27b/ }));
    expect(props.model.onSelectModel).toHaveBeenCalledWith(MODELS[1]);
  });

  it('opens the model settings dialog from "Model settings…"', async () => {
    renderTools(toolsProps());
    await openMore();
    fireEvent.click(screen.getByRole('menuitem', { name: 'Model settings…' }));
    // `LLMSettingsDialog` is a bare `Modal`: found by its title.
    expect(await screen.findByText('Model settings')).toBeInTheDocument();
  });

  it('hides the picker and "Model settings…" while a participant names the model', async () => {
    renderTools(toolsProps({ model: { ...toolsProps().model, shown: false } }));
    expect(screen.queryByTestId('model-selector-name')).not.toBeInTheDocument();
    await openMore();
    expect(screen.queryByRole('menuitem', { name: 'Model settings…' })).not.toBeInTheDocument();
  });

  it('toggles read aloud: play when off, stop when on', async () => {
    const props = toolsProps();
    renderTools(props);
    await openMore();
    const row = await screen.findByRole('menuitemcheckbox', { name: 'Read replies aloud' });
    expect(row).toHaveAttribute('aria-checked', 'false');
    fireEvent.click(row);
    expect(props.voice.onPlay).toHaveBeenCalledOnce();
  });

  it('stops reading when read aloud is on', async () => {
    const props = toolsProps({ voice: voiceProps(true) });
    renderTools(props);
    await openMore();
    fireEvent.click(await screen.findByRole('menuitemcheckbox', { name: 'Read replies aloud' }));
    expect(props.voice.onStop).toHaveBeenCalledOnce();
  });

  it('opens the voice settings dialog from "Voice settings…"', async () => {
    renderTools(toolsProps());
    await openMore();
    fireEvent.click(await screen.findByTestId('chat-voice-settings-button'));
    await waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());
  });

  it('clears the history through the same onClear, and is disabled when the chat is', async () => {
    const props = toolsProps();
    renderTools(props);
    await openMore();
    fireEvent.click(screen.getByTestId('chat-clear-history'));
    expect(props.clearChat.onClear).toHaveBeenCalledOnce();
  });

  it('keeps "Clear chat history…" disabled with nothing to clear', async () => {
    renderTools(toolsProps({ clearChat: { disabled: true, onClear: vi.fn() } }));
    await openMore();
    expect(screen.getByTestId('chat-clear-history')).toHaveAttribute('aria-disabled', 'true');
  });
});
