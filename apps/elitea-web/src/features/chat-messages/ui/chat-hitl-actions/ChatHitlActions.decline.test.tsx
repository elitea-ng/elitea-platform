/**
 * #950 (ELITEA-1013) — declining a sensitive call with the comment box left
 * empty.
 *
 * The continuation route's contract is deliberate and unchanged here:
 * `reject` refuses a value, `block_with_comment` requires one
 * (`agentexecution/continue.go`'s `validCurrentHITLDecision`). What was wrong
 * was the card, which always sent `block_with_comment` — so the one path a
 * user takes without thinking (click Reject, type nothing) was answered with
 * `400 Invalid agent execution request` and the pause stayed open with
 * nothing said. These assertions are on the PAYLOAD the card emits, which is
 * exactly what `buildHitlContinueBody` encodes onto the wire.
 */
import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { ChatHitlActions } from './ChatHitlActions';
import type { HitlInterrupt, HitlResumePayload } from './ChatHitlActions';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/** The pause a sensitive tool raises, in the shape the runtime stores it (`events.rs`). */
const SENSITIVE_INTERRUPT: HitlInterrupt = {
  message: 'mock_tool_create requires authorization',
  guardrail_type: 'sensitive_tool',
  available_actions: ['approve', 'reject', 'block_with_comment'],
  tool_name: 'mock_tool_create',
  tool_call_id: 'call_mock_tool_create_1',
};

function renderCard(onHitlResume: (payload: HitlResumePayload) => void, interrupt = SENSITIVE_INTERRUPT) {
  render(
    <ThemeProvider theme={theme}>
      <ChatHitlActions
        hitlInterrupt={interrupt}
        toolCallId={interrupt.tool_call_id ?? ''}
        onHitlResume={onHitlResume}
      />
    </ThemeProvider>,
  );
}

describe('declining a sensitive call', () => {
  it('sends a plain reject when no comment was typed', () => {
    const onHitlResume = vi.fn();
    renderCard(onHitlResume);

    fireEvent.click(screen.getByRole('button', { name: 'Reject' }));

    expect(onHitlResume).toHaveBeenCalledTimes(1);
    const payload = onHitlResume.mock.calls[0]?.[0] as HitlResumePayload;
    expect(payload.action, 'an empty comment is a plain reject, not a valueless block_with_comment').toBe('reject');
    expect(payload.value ?? '').toBe('');
    expect(payload.toolCallId).toBe('call_mock_tool_create_1');
  });

  it('sends block_with_comment, verbatim, when a comment WAS typed', () => {
    const onHitlResume = vi.fn();
    renderCard(onHitlResume);

    fireEvent.change(screen.getByPlaceholderText('Add a comment (optional)...'), {
      target: { value: '  not in production  ' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Reject' }));

    const payload = onHitlResume.mock.calls[0]?.[0] as HitlResumePayload;
    expect(payload.action).toBe('block_with_comment');
    expect(payload.value, 'the comment must travel unchanged, not trimmed').toBe('  not in production  ');
  });

  it('keeps block_with_comment for a pause that offers no plain reject', () => {
    const onHitlResume = vi.fn();
    renderCard(onHitlResume, { ...SENSITIVE_INTERRUPT, available_actions: ['approve', 'block_with_comment'] });

    fireEvent.click(screen.getByRole('button', { name: 'Reject' }));

    const payload = onHitlResume.mock.calls[0]?.[0] as HitlResumePayload;
    expect(payload.action).toBe('block_with_comment');
  });
});
