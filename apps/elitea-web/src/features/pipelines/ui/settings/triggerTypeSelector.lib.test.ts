import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { UsePipelineTriggersResult } from '../../api/usePipelineTriggers';
import {
  TRIGGER_TYPES,
  buildTriggerTooltip,
  computeHasInteractiveElements,
  buildTriggerList,
  currentTriggerKind,
  useAutoResetTriggerOnInteractive,
  useTriggerActions,
} from './triggerTypeSelector.lib';

describe('computeHasInteractiveElements', () => {
  it('returns false for undefined instructions', () => {
    expect(computeHasInteractiveElements(undefined)).toBe(false);
  });

  it('returns false for unparsable YAML', () => {
    expect(computeHasInteractiveElements('{{{not: valid: yaml')).toBe(false);
  });

  it('returns false when the parsed YAML is a literal null document (parses successfully, but falsy — distinct from the parse-failure catch branch)', () => {
    expect(computeHasInteractiveElements('null')).toBe(false);
  });

  it('returns false when there are no interactive nodes or interrupts', () => {
    expect(computeHasInteractiveElements('nodes:\n  - id: a\n    type: agent\n')).toBe(false);
  });

  it('returns true when a node has an interactive type (hitl)', () => {
    expect(computeHasInteractiveElements('nodes:\n  - id: a\n    type: hitl\n')).toBe(true);
  });

  it('returns true when a node has an interactive type (printer)', () => {
    expect(computeHasInteractiveElements('nodes:\n  - id: a\n    type: printer\n')).toBe(true);
  });

  it('returns true when interrupt_before is non-empty', () => {
    expect(computeHasInteractiveElements('nodes: []\ninterrupt_before:\n  - a\n')).toBe(true);
  });

  it('returns true when interrupt_after is non-empty', () => {
    expect(computeHasInteractiveElements('nodes: []\ninterrupt_after:\n  - a\n')).toBe(true);
  });
});

describe('currentTriggerKind', () => {
  it('reads Chat Message as the absence of both facilities', () => {
    expect(currentTriggerKind({ hasSchedule: false, hasWebhook: false })).toBe(TRIGGER_TYPES.chat_message);
  });

  it('shows the schedule when only a schedule exists', () => {
    expect(currentTriggerKind({ hasSchedule: true, hasWebhook: false })).toBe(TRIGGER_TYPES.schedule);
  });

  it('shows the webhook when only a webhook exists', () => {
    expect(currentTriggerKind({ hasSchedule: false, hasWebhook: true })).toBe(TRIGGER_TYPES.webhook);
  });

  /** Both CAN exist — the backend has two independent facilities — and the single-valued control shows the one that starts runs by itself. The other is still listed. */
  it('prefers the schedule when both exist', () => {
    expect(currentTriggerKind({ hasSchedule: true, hasWebhook: true })).toBe(TRIGGER_TYPES.schedule);
  });
});

describe('buildTriggerList', () => {
  it('lists nothing when neither facility is configured', () => {
    expect(buildTriggerList({ hasSchedule: false, cron: undefined, hasWebhook: false, webhookUrl: undefined })).toEqual([]);
  });

  it('lists both kinds, schedule first, each with its own detail', () => {
    expect(buildTriggerList({ hasSchedule: true, cron: '0 9 * * 1', hasWebhook: true, webhookUrl: '/api/v2/pipeline_trigger/1/tok' })).toEqual([
      { kind: 'schedule', label: 'Schedule', detail: '0 9 * * 1' },
      { kind: 'webhook', label: 'Webhook', detail: '/api/v2/pipeline_trigger/1/tok' },
    ]);
  });

  it('renders an empty detail rather than "undefined" when the backend omits the field', () => {
    expect(buildTriggerList({ hasSchedule: true, cron: undefined, hasWebhook: false, webhookUrl: undefined })).toEqual([
      { kind: 'schedule', label: 'Schedule', detail: '' },
    ]);
  });
});

describe('buildTriggerTooltip', () => {
  it('returns just the base tooltip when there are no interactive elements', () => {
    const tooltip = buildTriggerTooltip(false);
    expect(tooltip).toContain('Choose how this pipeline is triggered.');
    expect(tooltip).not.toContain('Note:');
  });

  it('appends the interactive-elements note when there are interactive elements', () => {
    const tooltip = buildTriggerTooltip(true);
    expect(tooltip).toContain('Choose how this pipeline is triggered.');
    expect(tooltip).toContain('Note: This pipeline contains HITL, Printer nodes, or interrupts');
  });
});

describe('useAutoResetTriggerOnInteractive', () => {
  function setup(overrides: Partial<Parameters<typeof useAutoResetTriggerOnInteractive>[0]> = {}) {
    const removeAll = vi.fn().mockResolvedValue(undefined);
    const onNotifySuccess = vi.fn();
    const onNotifyError = vi.fn();
    const initialProps = {
      hasInteractiveElements: false,
      kinds: { hasSchedule: false, hasWebhook: false },
      removeAll,
      onNotifySuccess,
      onNotifyError,
      ...overrides,
    };
    const rendered = renderHook((props: typeof initialProps) => useAutoResetTriggerOnInteractive(props), { initialProps });
    return { ...rendered, removeAll, onNotifySuccess, onNotifyError, initialProps };
  }

  it('does nothing when it was already interactive on mount (no true transition edge)', () => {
    const { removeAll } = setup({ hasInteractiveElements: true, kinds: { hasSchedule: true, hasWebhook: false } });
    expect(removeAll).not.toHaveBeenCalled();
  });

  it('does nothing on a transition to interactive when nothing unattended is configured', () => {
    const { rerender, removeAll, initialProps } = setup();
    rerender({ ...initialProps, hasInteractiveElements: true });
    expect(removeAll).not.toHaveBeenCalled();
  });

  it('removes a configured schedule and reports success when the graph becomes interactive', async () => {
    const { rerender, removeAll, onNotifySuccess, onNotifyError, initialProps } = setup({
      kinds: { hasSchedule: true, hasWebhook: false },
    });

    await act(async () => {
      rerender({ ...initialProps, hasInteractiveElements: true });
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(removeAll).toHaveBeenCalledOnce();
    expect(onNotifySuccess).toHaveBeenCalledWith(
      'Trigger reset to Chat Message (pipeline now contains interactive elements)',
    );
    expect(onNotifyError).not.toHaveBeenCalled();
  });

  it('removes a configured webhook too', async () => {
    const { rerender, removeAll, initialProps } = setup({ kinds: { hasSchedule: false, hasWebhook: true } });

    await act(async () => {
      rerender({ ...initialProps, hasInteractiveElements: true });
      await Promise.resolve();
    });

    expect(removeAll).toHaveBeenCalledOnce();
  });

  it('reports an error when the removal fails', async () => {
    const removeAll = vi.fn().mockRejectedValue(new Error('boom'));
    const onNotifyError = vi.fn();
    const onNotifySuccess = vi.fn();
    const initialProps = {
      hasInteractiveElements: false,
      kinds: { hasSchedule: true, hasWebhook: false },
      removeAll,
      onNotifySuccess,
      onNotifyError,
    };
    const { rerender } = renderHook((props: typeof initialProps) => useAutoResetTriggerOnInteractive(props), { initialProps });

    await act(async () => {
      rerender({ ...initialProps, hasInteractiveElements: true });
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(onNotifyError).toHaveBeenCalledWith('Failed to reset trigger');
    expect(onNotifySuccess).not.toHaveBeenCalled();
  });
});

describe('useTriggerActions', () => {
  function makeTriggers(overrides: Partial<UsePipelineTriggersResult> = {}) {
    return {
      schedule: undefined,
      webhook: undefined,
      isFetching: false,
      saveSchedule: vi.fn().mockResolvedValue(undefined),
      removeSchedule: vi.fn().mockResolvedValue(undefined),
      rotateWebhook: vi.fn().mockResolvedValue({ configured: true, secret: 'minted' }),
      revealWebhook: vi.fn().mockResolvedValue({ configured: true, secret: 'revealed' }),
      removeWebhook: vi.fn().mockResolvedValue(undefined),
      ...overrides,
    } as unknown as UsePipelineTriggersResult;
  }

  function setup(options: { triggers?: UsePipelineTriggersResult; kinds?: { hasSchedule: boolean; hasWebhook: boolean } } = {}) {
    const triggers = options.triggers ?? makeTriggers();
    const setIsUpdating = vi.fn();
    const setIsScheduleModalOpen = vi.fn();
    const setIsWebhookModalOpen = vi.fn();
    const setRevealedSecret = vi.fn();
    const onNotifySuccess = vi.fn();
    const onNotifyError = vi.fn();
    const args = {
      triggers,
      kinds: options.kinds ?? { hasSchedule: false, hasWebhook: false },
      setIsUpdating,
      setIsScheduleModalOpen,
      setIsWebhookModalOpen,
      setRevealedSecret,
      onNotifySuccess,
      onNotifyError,
    };
    const { result } = renderHook(() => useTriggerActions(args));
    return { result, triggers, setIsUpdating, setIsScheduleModalOpen, setIsWebhookModalOpen, setRevealedSecret, onNotifySuccess, onNotifyError };
  }

  describe('handleTriggerTypeChange', () => {
    it('opens the schedule modal (writing nothing) for schedule', async () => {
      const { result, triggers, setIsScheduleModalOpen } = setup();
      await result.current.handleTriggerTypeChange(TRIGGER_TYPES.schedule);
      expect(setIsScheduleModalOpen).toHaveBeenCalledWith(true);
      expect(triggers.saveSchedule).not.toHaveBeenCalled();
    });

    it('creates the trigger, keeps the minted secret, and opens the webhook modal', async () => {
      const { result, triggers, setIsUpdating, setIsWebhookModalOpen, setRevealedSecret } = setup();
      await result.current.handleTriggerTypeChange(TRIGGER_TYPES.webhook);
      expect(triggers.rotateWebhook).toHaveBeenCalledOnce();
      expect(setRevealedSecret).toHaveBeenCalledWith('minted');
      expect(setIsWebhookModalOpen).toHaveBeenCalledWith(true);
      expect(setIsUpdating).toHaveBeenCalledWith(true);
      expect(setIsUpdating).toHaveBeenLastCalledWith(false);
    });

    /** Rotation is TOTAL — it kills the secret somebody else is already using — so selecting Webhook on a pipeline that already has one must not silently rotate it. */
    it('opens the modal without rotating when a trigger already exists', async () => {
      const { result, triggers, setIsWebhookModalOpen } = setup({ kinds: { hasSchedule: false, hasWebhook: true } });
      await result.current.handleTriggerTypeChange(TRIGGER_TYPES.webhook);
      expect(triggers.rotateWebhook).not.toHaveBeenCalled();
      expect(setIsWebhookModalOpen).toHaveBeenCalledWith(true);
    });

    it('reports the backend error text (not a fixed generic message) when the create fails', async () => {
      const triggers = makeTriggers({ rotateWebhook: vi.fn().mockRejectedValue(new Error('webhook quota exceeded')) });
      const { result, setIsWebhookModalOpen, onNotifyError } = setup({ triggers });
      await result.current.handleTriggerTypeChange(TRIGGER_TYPES.webhook);
      expect(onNotifyError).toHaveBeenCalledWith('webhook quota exceeded');
      expect(setIsWebhookModalOpen).not.toHaveBeenCalled();
    });

    it('falls back to the generic message for a non-Error/non-string rejection', async () => {
      const triggers = makeTriggers({ rotateWebhook: vi.fn().mockRejectedValue({ some: 'object' }) });
      const { result, onNotifyError } = setup({ triggers });
      await result.current.handleTriggerTypeChange(TRIGGER_TYPES.webhook);
      expect(onNotifyError).toHaveBeenCalledWith('Failed to configure webhook');
    });

    it('removes every configured facility when Chat Message is chosen', async () => {
      const { result, triggers, onNotifySuccess } = setup({ kinds: { hasSchedule: true, hasWebhook: true } });
      await result.current.handleTriggerTypeChange(TRIGGER_TYPES.chat_message);
      expect(triggers.removeSchedule).toHaveBeenCalledOnce();
      expect(triggers.removeWebhook).toHaveBeenCalledOnce();
      expect(onNotifySuccess).toHaveBeenCalledWith('Trigger updated to Chat Message');
    });

    it('reports the backend error text when the removal fails', async () => {
      const triggers = makeTriggers({ removeSchedule: vi.fn().mockRejectedValue(new Error('version is locked')) });
      const { result, onNotifyError } = setup({ triggers, kinds: { hasSchedule: true, hasWebhook: false } });
      await result.current.handleTriggerTypeChange(TRIGGER_TYPES.chat_message);
      expect(onNotifyError).toHaveBeenCalledWith('version is locked');
    });
  });

  describe('handleScheduleSubmit', () => {
    it('saves the cron expression, reports success, and tracks isUpdating', async () => {
      const { result, triggers, setIsUpdating, onNotifySuccess } = setup();
      await result.current.handleScheduleSubmit('0 9 * * 1');
      expect(triggers.saveSchedule).toHaveBeenCalledWith('0 9 * * 1');
      expect(onNotifySuccess).toHaveBeenCalledWith('Schedule configured successfully');
      expect(setIsUpdating).toHaveBeenCalledWith(true);
      expect(setIsUpdating).toHaveBeenLastCalledWith(false);
    });

    it('reports the backend error text when the save fails, and still clears isUpdating', async () => {
      const triggers = makeTriggers({ saveSchedule: vi.fn().mockRejectedValue(new Error('invalid cron expression')) });
      const { result, setIsUpdating, onNotifyError } = setup({ triggers });
      await result.current.handleScheduleSubmit('0 9 * * 1');
      expect(onNotifyError).toHaveBeenCalledWith('invalid cron expression');
      expect(setIsUpdating).toHaveBeenCalledWith(true);
      expect(setIsUpdating).toHaveBeenLastCalledWith(false);
    });
  });

  describe('handleRevealWebhook', () => {
    it('keeps the revealed credential', async () => {
      const { result, triggers, setRevealedSecret } = setup({ kinds: { hasSchedule: false, hasWebhook: true } });
      await result.current.handleRevealWebhook();
      expect(triggers.revealWebhook).toHaveBeenCalledOnce();
      expect(setRevealedSecret).toHaveBeenCalledWith('revealed');
    });

    /** A 409 means the stored secret cannot be read; the only repair a person can perform is a rotation, so that is what the message says. */
    it('reports the backend text when the reveal is refused', async () => {
      const triggers = makeTriggers({ revealWebhook: vi.fn().mockRejectedValue({ not: 'an error' }) });
      const { result, onNotifyError } = setup({ triggers, kinds: { hasSchedule: false, hasWebhook: true } });
      await result.current.handleRevealWebhook();
      expect(onNotifyError).toHaveBeenCalledWith('Failed to reveal the webhook secret — rotate it to get a new one');
    });
  });

  describe('handleDeleteKind', () => {
    it('deletes the schedule alone', async () => {
      const { result, triggers, onNotifySuccess } = setup({ kinds: { hasSchedule: true, hasWebhook: true } });
      await result.current.handleDeleteKind('schedule');
      expect(triggers.removeSchedule).toHaveBeenCalledOnce();
      expect(triggers.removeWebhook).not.toHaveBeenCalled();
      expect(onNotifySuccess).toHaveBeenCalledWith('Schedule removed');
    });

    it('revokes the webhook, drops the held credential and closes its dialog', async () => {
      const { result, triggers, setRevealedSecret, setIsWebhookModalOpen, onNotifySuccess } = setup({ kinds: { hasSchedule: false, hasWebhook: true } });
      await result.current.handleDeleteKind('webhook');
      expect(triggers.removeWebhook).toHaveBeenCalledOnce();
      expect(setRevealedSecret).toHaveBeenCalledWith(undefined);
      expect(setIsWebhookModalOpen).toHaveBeenCalledWith(false);
      expect(onNotifySuccess).toHaveBeenCalledWith('Webhook revoked');
    });

    it('reports the backend error text when a delete fails', async () => {
      const triggers = makeTriggers({ removeWebhook: vi.fn().mockRejectedValue(new Error('already revoked')) });
      const { result, onNotifyError } = setup({ triggers, kinds: { hasSchedule: false, hasWebhook: true } });
      await result.current.handleDeleteKind('webhook');
      expect(onNotifyError).toHaveBeenCalledWith('already revoked');
    });
  });

  describe('removeAll', () => {
    it('touches only the facilities that exist', async () => {
      const { result, triggers } = setup({ kinds: { hasSchedule: false, hasWebhook: false } });
      await result.current.removeAll();
      expect(triggers.removeSchedule).not.toHaveBeenCalled();
      expect(triggers.removeWebhook).not.toHaveBeenCalled();
    });
  });
});
