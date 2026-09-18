/**
 * The composer's left-hand control, per surface.
 *
 * This exists because of the specific way the "+" menu was broken: nothing
 * was wrong with `PlusChatButton` itself — it was built, exported, and had
 * passing unit tests. It simply had no caller, so the chat composer showed a
 * paperclip and a gear where the product shows one "+", and the agent /
 * pipeline / toolkit / MCP submenus were unreachable. A test of the component
 * cannot see that. A test of the SLOT BUILDER — the thing that decides which
 * component the footer gets — can.
 *
 * The assertions read the returned ELEMENTS rather than mounting them. That
 * is not a shortcut around a hard render: `PlusChatButton` reaches for the
 * router context (via `useAvailableInternalTools` -> `useSelectedProjectId`),
 * so mounting it would mean standing up a router purely to re-check what
 * these assertions already establish. What is under test is which component
 * the builder chose and what it handed it — both of which live on the element.
 */
import type { ReactElement } from 'react';

import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { AttachmentButton, ChatInternalToolsConfigButton, PlusChatButton, VoiceButton } from '@/widgets/chat';
import { VoiceControlButton } from '@/features/chat-input';
import { LLMModelSelector } from '@/widgets/llm-model-selector';

import { buildChatBoxAttachmentProps, buildChatBoxInputSlots } from './ChatBoxInputSlots';

type ToolRow = { key: string; label: string; enabled: boolean };

/** Minimal `VoicePlayerProps` stand-in — same shape `VoiceControlButton.test.tsx`'s own `baseProps()` uses. */
function baseVoiceProps() {
  return {
    isPlaying: false,
    onPlay: vi.fn(),
    onStop: vi.fn(),
    voiceConfig: { voiceName: null, voiceId: null, rate: 1, volume: 1 },
    voices: [],
    onVoiceConfigChange: vi.fn(),
    ttsModel: null,
    hasModelTTS: false,
  };
}

function buildSlots(
  isAgentsPage: boolean,
  tools: ToolRow[] | undefined = [{ key: 'planner', label: 'Planner', enabled: true }],
  onToolChange: ((toolKey: string, enabled: boolean) => void) | undefined = vi.fn(),
  clearChat: { disabled: boolean; onClear: () => void } = { disabled: false, onClear: vi.fn() },
  /** A17: a turn is open, so every attachment path is refused (ELITEA-2867). */
  attachmentsDisabled = false,
) {
  return buildChatBoxInputSlots({
    attachments: { attachments: [], onAttachFiles: vi.fn(), disabled: attachmentsDisabled },
    internalTools: { disabled: false, tools, onToolChange },
    model: { llmSettings: undefined, onSetLLMSettings: undefined, selectedModel: undefined, onSelectModel: undefined, models: [] },
    clearChat,
    refs: { attachmentButtonRef: { current: null }, voiceButtonRef: { current: null }, voiceInputRef: { current: null } },
    voice: baseVoiceProps(),
    voiceInput: { onRecordingChange: vi.fn(), onError: vi.fn() },
    isAgentsPage,
    entitySubmenus: undefined,
    participants: undefined,
  });
}

/** `memo()` wraps the component, so compare against the element's own type. */
function typeOf(node: unknown): unknown {
  return (node as ReactElement).type;
}

describe("buildChatBoxInputSlots — the composer's left-hand control", () => {
  it('gives the chat surface the "+" menu, and withholds the separate modules gear', () => {
    const slots = buildSlots(false);

    expect(typeOf(slots.attachmentButton)).toBe(PlusChatButton);
    // The "+" menu contains the modules toggles itself (its "Modules"
    // submenu), so a second standalone gear beside it is the same control
    // twice. `NewChatInputFooterContent` renders this slot whenever
    // `!isAgentsPage`, so withholding it is the composition root's job.
    expect(slots.internalToolsConfig).toBeUndefined();
  });

  it('gives the agents page the plain paperclip and its own modules gear', () => {
    const slots = buildSlots(true);

    expect(typeOf(slots.attachmentButton)).toBe(AttachmentButton);
    expect(typeOf(slots.internalToolsConfig)).toBe(ChatInternalToolsConfigButton);
  });

  it('shows the execution-loop limit in regular chat model settings', () => {
    const slots = buildSlots(false);
    const selector = slots.modelSelector as ReactElement;

    expect(typeOf(selector)).toBe(LLMModelSelector);
    expect((selector.props as { showStepsLimit?: boolean }).showStepsLimit).toBe(true);
  });

  it('mounts the TTS gear/settings control (VoiceControlButton) beside the ASR mic control in the voiceButton slot (A9)', () => {
    // `VoiceControlButton` — the gear icon that opens `VoiceConfigDialog` —
    // had no render site anywhere in `src/` (ELITEA-1312/1313/1315). This
    // asserts the composition root now mounts it, not just that the
    // component itself works (its own test file already covers that).
    const slots = buildSlots(false);
    const voiceButton = slots.voiceButton as ReactElement;
    const children = (voiceButton.props as { children: ReactElement[] }).children;

    expect(typeOf(children[0])).toBe(VoiceButton);
    expect(typeOf(children[1])).toBe(VoiceControlButton);
  });
});

describe('buildChatBoxInputSlots — drop/paste attachment handle', () => {
  it('hands the "+" menu the attachment ref the drop/paste bridge dispatches through', () => {
    // `useNewChatInputAttachmentBridge` (features/chat-input) delivers dropped
    // and pasted files via `refs.attachmentButtonRef.current.onDrop(...)` and
    // silently no-ops while `current` is null. On the chat surface the "+"
    // menu is the left-hand control, so the builder must hand it this exact
    // ref (it mounts a hidden `AttachmentButton` with it) — omitting it left
    // every drop/paste on /chat discarded with no error.
    const attachmentButtonRef = { current: null };
    const slots = buildChatBoxInputSlots({
      attachments: { attachments: [], onAttachFiles: vi.fn(), disabled: false },
      internalTools: { disabled: false, tools: [], onToolChange: vi.fn() },
      model: { llmSettings: undefined, onSetLLMSettings: undefined, selectedModel: undefined, onSelectModel: undefined, models: [] },
      clearChat: { disabled: false, onClear: vi.fn() },
      refs: { attachmentButtonRef, voiceButtonRef: { current: null }, voiceInputRef: { current: null } },
      voice: baseVoiceProps(),
      voiceInput: { onRecordingChange: vi.fn(), onError: vi.fn() },
      isAgentsPage: false,
      entitySubmenus: undefined,
      participants: undefined,
    });

    const props = (slots.attachmentButton as ReactElement).props as { attachmentButtonRef?: unknown };
    expect(props.attachmentButtonRef).toBe(attachmentButtonRef);
  });
});

describe('buildChatBoxInputSlots — the staged-file chips', () => {
  it('fills the attachmentList slot with real, deletable chips', async () => {
    // The counterpart to the "+"-menu case above, and the same class of
    // defect: `UserInput` renders staged files through `slots.attachmentList`,
    // `FileList` was written to fill it, and nothing ever supplied it. The
    // composer counted a picked file (the menu's "N left" ticked down) and
    // uploaded it on send, but showed the user no chip and no way to remove it.
    const slots = buildSlots(false);
    const onDeleteAttachment = vi.fn();
    const file = new File(['x'], 'brief.txt');

    render(<>{slots.attachmentList({ attachments: [file], onDeleteAttachment, disabled: false })}</>);

    expect(screen.getByTestId('chat-attachment-chip-0')).toHaveTextContent('brief.txt');
    await userEvent.click(screen.getByTestId('chat-attachment-remove-0'));
    expect(onDeleteAttachment).toHaveBeenCalledWith(0);
  });

  it('withholds the delete affordance while the turn is in flight', () => {
    const slots = buildSlots(false);
    const file = new File(['x'], 'brief.txt');

    render(<>{slots.attachmentList({ attachments: [file], onDeleteAttachment: vi.fn(), disabled: true })}</>);

    expect(screen.queryByTestId('chat-attachment-remove-0')).toBeNull();
  });
});

describe('buildChatBoxInputSlots — internal-tools shape translation', () => {
  it('hands the "+" menu the ENABLED tool keys, and routes its callback back', () => {
    // `ChatInternalToolsConfigButton` takes `{key,label,enabled}` rows plus a
    // `(key, enabled)` callback; `PlusChatButton` takes the enabled NAMES plus
    // a `({key, value})` one. `key` here IS the tool's `name`
    // (`useChatBoxInternalTools` builds it from `tool.name`), so this is a
    // shape translation and must not drop or re-key anything.
    const onToolChange = vi.fn();
    const slots = buildSlots(
      false,
      [
        { key: 'planner', label: 'Planner', enabled: true },
        { key: 'sandbox', label: 'Python Sandbox', enabled: false },
      ],
      onToolChange,
    );

    const props = (slots.attachmentButton as ReactElement).props as {
      internal_tools: string[];
      onInternalToolsConfigChange: (c: { key: string; value: boolean }) => void;
    };

    expect(props.internal_tools).toEqual(['planner']);

    props.onInternalToolsConfigChange({ key: 'sandbox', value: true });
    expect(onToolChange).toHaveBeenCalledWith('sandbox', true);
  });
});

describe('buildChatBoxInputSlots — the conversation-surface clear-history control (G1)', () => {
  /*
   * The defect this closes is a composition one, and only a test of the
   * BUILDER can see it: `ChatBox` exposed `onClear` on its imperative handle,
   * `useChatBoxActions.handleClear` opened the delete-all confirmation, and
   * `useDeleteMessageAlert`'s `ALL_MESSAGES` sentinel routed the confirm to
   * the DELETE. Every one of those had passing unit tests. The only caller of
   * the handle was the pipeline editor's test-chat panel, so on `/chat` the
   * whole mechanism was unreachable and a user could not empty a
   * conversation at all. A test of any single piece stays green through that.
   */
  it('gives the chat surface a clear control wired to the delete-all handler', async () => {
    const onClear = vi.fn();
    const slots = buildSlots(false, undefined, undefined, { disabled: false, onClear });

    render(<>{slots.clearChat}</>);

    const control = screen.getByTestId('chat-clear-history');
    // Named after what it destroys, so the accessible name is not the
    // scratch-transcript "Clear chat" the test panels use.
    expect(control).toHaveAccessibleName('Clear the chat history');
    await userEvent.click(control);
    expect(onClear).toHaveBeenCalledTimes(1);
  });

  it('refuses the click while the transcript is empty or a turn is running', () => {
    // Baseline `shouldDisableClear` (`!chat_history.length || isStreaming`).
    // `ChatBox` also guards `handleClear` itself, so a control that stayed
    // enabled would still not delete anything — it would open nothing and
    // look broken, which is why the disabled state is asserted here rather
    // than assumed from the guard.
    const onClear = vi.fn();
    const slots = buildSlots(false, undefined, undefined, { disabled: true, onClear });

    render(<>{slots.clearChat}</>);

    const control = screen.getByTestId('chat-clear-history');
    expect(control).toBeDisabled();
    // Fired directly rather than through `userEvent`, which refuses to click
    // an element whose `pointer-events` MUI has already turned off — the
    // refusal proves the styling, not the handler. Dispatching the event the
    // browser would deliver is what discriminates a control that is merely
    // greyed out from one that is actually inert.
    fireEvent.click(control);
    expect(onClear).not.toHaveBeenCalled();
  });

  it('withholds it on the agent/pipeline editor surface, which renders its own', () => {
    // `features/pipelines/ui/ChatPanel.tsx` fills `renderClearChatButton`
    // beside the panel. Supplying one here too would put two clear controls
    // on the same screen — the mirror of the modules-gear case above.
    expect(buildSlots(true, undefined, undefined, { disabled: false, onClear: vi.fn() }).clearChat).toBeUndefined();
  });
});

/*
 * A17 (ELITEA-2867): attachments are refused for the whole of an open run, and
 * `disableAttachments` is the ONE switch that covers every way in — the "+"
 * menu's Attach Files row, the bare paperclip, and the drop/paste bridge, which
 * delivers through `attachmentButtonRef.current.onDrop(...)` and is gated on
 * the same flag inside `AttachmentButton`. Gating only the visible button would
 * leave drag-and-drop and Ctrl+V attaching to a turn already in flight, which
 * is precisely what the case calls a failure.
 */
describe('attachments while a turn is open', () => {
  it('disables the chat surface "+" menu', () => {
    const slots = buildSlots(false);
    expect((slots.attachmentButton as ReactElement<{ disableAttachments?: boolean }>).props.disableAttachments).toBe(false);
    const running = buildSlots(false, undefined, undefined, undefined, true);
    expect((running.attachmentButton as ReactElement<{ disableAttachments?: boolean }>).props.disableAttachments).toBe(true);
  });

  it('disables the agents-page paperclip', () => {
    const running = buildSlots(true, undefined, undefined, undefined, true);
    expect((running.attachmentButton as ReactElement<{ disableAttachments?: boolean }>).props.disableAttachments).toBe(true);
  });

  it('tells the composer to refuse drop and paste too', () => {
    const state = { attachments: [], onAttachFiles: vi.fn(), onDeleteAttachment: vi.fn() };
    const upload = { isUploading: false, uploadProgress: 0 };
    // `NewChatInput`'s `useNewChatInputAttachmentBridge` reads exactly this
    // field; without it the bridge stays open while a run streams.
    expect(buildChatBoxAttachmentProps({ state, upload }).disabled).toBe(false);
    expect(buildChatBoxAttachmentProps({ state, upload }, true).disabled).toBe(true);
  });
});
