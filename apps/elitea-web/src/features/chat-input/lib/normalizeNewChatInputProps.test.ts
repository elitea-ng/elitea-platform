import { describe, expect, it, vi } from 'vitest';

import {
  resolveAttachments,
  resolveCallbacks,
  resolveContent,
  resolveMentions,
  resolveRefs,
  resolveSlots,
  resolveState,
  resolveVoice,
} from './normalizeNewChatInputProps';

describe('resolveState', () => {
  it('defaults every field to false', () => {
    expect(resolveState(undefined)).toEqual({
      isLoading: false,
      isStreaming: false,
      disabledSend: false,
      isCreatingConversation: false,
      isEditorDirty: false,
    });
  });
  it('passes through given values', () => {
    expect(resolveState({ isLoading: true, isStreaming: true })).toMatchObject({ isLoading: true, isStreaming: true });
  });
});

describe('resolveContent', () => {
  it('defaults placeholder to "", clearInputAfterSubmit to true, and highlights to []', () => {
    expect(resolveContent(undefined)).toEqual({
      placeholder: '',
      clearInputAfterSubmit: true,
      tooltipOfSendButton: undefined,
      slashHighlights: [],
    });
  });
});

describe('resolveCallbacks', () => {
  it('resolves every field, defined or not', () => {
    const onSend = vi.fn();
    expect(resolveCallbacks({ onSend })).toEqual({
      onSend,
      onStopGeneration: undefined,
      onNormalKeyDown: undefined,
      onInputChange: undefined,
    });
  });
});

describe('resolveAttachments', () => {
  it('defaults every field', () => {
    expect(resolveAttachments(undefined)).toEqual({
      items: [],
      onAttachFiles: undefined,
      onDeleteAttachment: undefined,
      disabled: false,
      isUploading: false,
      uploadProgress: 0,
    });
  });
});

describe('resolveMentions', () => {
  it('defaults users to []', () => {
    expect(resolveMentions(undefined)).toEqual({ users: [], onMentionChange: undefined });
  });
});

describe('resolveVoice', () => {
  it('defaults every field', () => {
    expect(resolveVoice(undefined)).toEqual({
      isSpeakingMode: false,
      onSpeakingModeToggle: undefined,
      isTTSPlaying: false,
      isRecording: false,
    });
  });
});

describe('resolveSlots / resolveRefs', () => {
  it('resolve every field, defined or not', () => {
    // Enumerated on purpose, and kept in step by hand: `resolveSlots`
    // rebuilds the bundle key by key, so a slot missing from it is dropped
    // between the composition root that supplies it and the footer that
    // renders it, with nothing anywhere reporting a problem.
    expect(resolveSlots(undefined)).toEqual({
      sendControl: undefined,
      highlightOverlay: undefined,
      attachmentList: undefined,
      attachmentButton: undefined,
      internalToolsConfig: undefined,
      clearChat: undefined,
      voiceButton: undefined,
      modelSelector: undefined,
    });
    expect(resolveRefs(undefined)).toEqual({ attachmentButtonRef: undefined, voiceButtonRef: undefined });
  });
});
