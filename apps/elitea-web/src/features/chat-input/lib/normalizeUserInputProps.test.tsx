import { describe, expect, it, vi } from 'vitest';

import {
  renderAttachmentList,
  resolveAttachmentsProps,
  resolveCallbacks,
  resolveField,
  resolveFooterControls,
  resolveHighlightRanges,
  resolveMention,
  resolveTooltip,
  resolveVoiceProps,
} from './normalizeUserInputProps';

describe('resolveField', () => {
  it('defaults every field to undefined when absent', () => {
    expect(resolveField(undefined)).toEqual({ placeholder: undefined, color: undefined, iconColor: undefined });
  });
  it('passes through given values', () => {
    expect(resolveField({ placeholder: 'Ask me', color: 'test-color-1', iconColor: 'test-color-2' })).toEqual({
      placeholder: 'Ask me',
      color: 'test-color-1',
      iconColor: 'test-color-2',
    });
  });
});

describe('resolveMention', () => {
  it('defaults users to an empty array', () => {
    expect(resolveMention(undefined)).toEqual({ users: [], onMentionChange: undefined });
  });
  it('passes through given users/callback', () => {
    const onMentionChange = vi.fn();
    const users = [{ name: 'Alice' }];
    expect(resolveMention({ users, onMentionChange })).toEqual({ users, onMentionChange });
  });
});

describe('resolveTooltip', () => {
  it('defaults to an empty title and top placement', () => {
    expect(resolveTooltip(undefined)).toEqual({ title: '', placement: 'top' });
  });
  it('passes through given values', () => {
    expect(resolveTooltip({ title: 'Hi', placement: 'bottom' })).toEqual({ title: 'Hi', placement: 'bottom' });
  });
});

describe('resolveHighlightRanges', () => {
  it('defaults to an empty array', () => {
    expect(resolveHighlightRanges(undefined)).toEqual([]);
  });
  it('passes through given ranges', () => {
    const ranges = [{ start: 0, end: 1 }];
    expect(resolveHighlightRanges({ ranges })).toBe(ranges);
  });
});

describe('resolveAttachmentsProps', () => {
  it('defaults every field', () => {
    expect(resolveAttachmentsProps(undefined)).toEqual({ items: [], onDelete: undefined, isUploading: false, uploadProgress: 0 });
  });
  it('passes through given values', () => {
    const onDelete = vi.fn();
    const items = [new File(['x'], 'a.txt')];
    expect(resolveAttachmentsProps({ items, onDelete, isUploading: true, uploadProgress: 42 })).toEqual({
      items,
      onDelete,
      isUploading: true,
      uploadProgress: 42,
    });
  });
});

describe('resolveVoiceProps', () => {
  it('defaults every field', () => {
    expect(resolveVoiceProps(undefined)).toEqual({
      isSpeakingMode: false,
      isRecording: false,
      onEnterSpeakingMode: undefined,
      onExitSpeakingMode: undefined,
    });
  });
});

describe('resolveCallbacks', () => {
  it('resolves every field, defined or not', () => {
    const onSend = vi.fn();
    expect(resolveCallbacks({ onSend })).toEqual({
      onSend,
      onStop: undefined,
      onNormalKeyDown: undefined,
      onInputChange: undefined,
      onFilePaste: undefined,
    });
    expect(resolveCallbacks(undefined)).toEqual({
      onSend: undefined,
      onStop: undefined,
      onNormalKeyDown: undefined,
      onInputChange: undefined,
      onFilePaste: undefined,
    });
  });
});

describe('renderAttachmentList', () => {
  it('renders nothing when there are no items', () => {
    const attachmentList = vi.fn();
    expect(renderAttachmentList([], attachmentList, undefined, false)).toBeNull();
    expect(attachmentList).not.toHaveBeenCalled();
  });

  it('invokes the slot with the resolved props when there are items', () => {
    const attachmentList = vi.fn(() => 'node');
    const onDeleteAttachment = vi.fn();
    const file = new File(['x'], 'a.txt');
    const result = renderAttachmentList([file], attachmentList, onDeleteAttachment, true);
    expect(attachmentList).toHaveBeenCalledWith({ attachments: [file], onDeleteAttachment, disabled: true });
    expect(result).toBe('node');
  });

  it('returns null when the slot itself is not provided', () => {
    expect(renderAttachmentList([new File(['x'], 'a.txt')], undefined, undefined, false)).toBeNull();
  });
});

describe('resolveFooterControls', () => {
  const running = { isStreaming: true, isUploading: false };
  it('shows Send alone while no turn runs', () => {
    expect(resolveFooterControls({ isStreaming: false, isUploading: false, keepSendWhileStreaming: true, oneButton: true })).toEqual({ showStop: false, showSend: true });
  });
  it('shows Stop alone during a turn for a host that does not queue', () => {
    expect(resolveFooterControls({ ...running, keepSendWhileStreaming: undefined, oneButton: true, hasText: true })).toEqual({ showStop: true, showSend: false });
  });
  it('shows both on the web for a host that queues', () => {
    expect(resolveFooterControls({ ...running, keepSendWhileStreaming: true })).toEqual({ showStop: true, showSend: true });
  });
  it('shows one round button on the desktop: Stop while the input is empty, Send once it holds text', () => {
    expect(resolveFooterControls({ ...running, keepSendWhileStreaming: true, oneButton: true, hasText: false })).toEqual({ showStop: true, showSend: false });
    expect(resolveFooterControls({ ...running, keepSendWhileStreaming: true, oneButton: true, hasText: true })).toEqual({ showStop: false, showSend: true });
  });
  it('keeps Send, not Stop, while a file uploads', () => {
    expect(resolveFooterControls({ isStreaming: true, isUploading: true, keepSendWhileStreaming: true, oneButton: true })).toEqual({ showStop: false, showSend: true });
  });
});
