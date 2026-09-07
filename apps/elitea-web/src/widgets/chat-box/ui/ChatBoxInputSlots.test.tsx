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

import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { AttachmentButton, ChatInternalToolsConfigButton, PlusChatButton } from '@/widgets/chat';
import { LLMModelSelector } from '@/widgets/llm-model-selector';

import { buildChatBoxInputSlots } from './ChatBoxInputSlots';

type ToolRow = { key: string; label: string; enabled: boolean };

function buildSlots(isAgentsPage: boolean, tools: ToolRow[] = [{ key: 'planner', label: 'Planner', enabled: true }], onToolChange = vi.fn()) {
  return buildChatBoxInputSlots({
    attachments: { attachments: [], onAttachFiles: vi.fn() },
    internalTools: { disabled: false, tools, onToolChange },
    model: { llmSettings: undefined, onSetLLMSettings: undefined, selectedModel: undefined, onSelectModel: undefined, models: [] },
    refs: { attachmentButtonRef: { current: null }, voiceButtonRef: { current: null }, voiceInputRef: { current: null } },
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
      attachments: { attachments: [], onAttachFiles: vi.fn() },
      internalTools: { disabled: false, tools: [], onToolChange: vi.fn() },
      model: { llmSettings: undefined, onSetLLMSettings: undefined, selectedModel: undefined, onSelectModel: undefined, models: [] },
      refs: { attachmentButtonRef, voiceButtonRef: { current: null }, voiceInputRef: { current: null } },
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
