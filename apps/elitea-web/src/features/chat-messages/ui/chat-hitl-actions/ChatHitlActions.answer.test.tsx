/**
 * The `answer` branch of `ChatHitlActions` — the clarification card the native
 * runtime's `ask_user` pause needs.
 *
 * WHAT THIS RULES OUT, measured in a live browser before the branch existed:
 * a pause whose `available_actions` is `['answer']` matched none of the
 * approve/reject/edit branches, so the card rendered the question TEXT and
 * ZERO controls. Nothing failed, nothing logged, and the run stayed paused
 * forever. So the assertions below are about CONTROLS and about the VALUE they
 * produce, not about the question text — which the broken card printed too.
 *
 * The submitted string is asserted as PARSED JSON: it is what
 * `buildHitlContinueBody` decodes into the structured `hitl_value` the
 * continuation route canonicalises and `AskUserRequest::format_answer` renders
 * back into the tool result the model reads.
 */
import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { ChatHitlActions } from './ChatHitlActions';
import type { HitlInterrupt, HitlResumePayload } from './ChatHitlActions';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/** The pause the mock `ask_user` script produces, in the shape the runtime stores it. */
const ASK_USER_INTERRUPT: HitlInterrupt = {
  message: 'Which environment should I target?',
  guardrail_type: 'clarifying_question',
  available_actions: ['answer'],
  tool_name: 'ask_user',
  tool_call_id: 'call_mock_ask_user_1',
  questions: [
    {
      id: 'environment',
      question: 'Which environment should I target?',
      header: 'Environment',
      options: [
        { label: 'Staging', description: 'the shared pre-production stack' },
        { label: 'Production', description: 'the live stack' },
      ],
      multiSelect: false,
      allow_other: true,
    },
  ],
};

function renderCard(
  hitlInterrupt: HitlInterrupt,
  onHitlResume: (payload: HitlResumePayload) => void,
): void {
  render(
    <ThemeProvider theme={theme}>
      <ChatHitlActions
        hitlInterrupt={hitlInterrupt}
        toolCallId={hitlInterrupt.tool_call_id ?? ''}
        onHitlResume={onHitlResume}
      />
    </ThemeProvider>,
  );
}

describe('ChatHitlActions — the ask_user clarification card', () => {
  it('renders one control per option and answers with the chosen label, keyed by question id', () => {
    const onHitlResume = vi.fn();
    renderCard(ASK_USER_INTERRUPT, onHitlResume);

    // The card, its question, and — the part the broken version had none of —
    // a control per option plus a submit.
    expect(screen.getByTestId('chat-hitl-actions')).toBeTruthy();
    expect(screen.getByTestId('hitl-answer-question-0').textContent).toContain(
      'Which environment should I target?',
    );
    expect(screen.getByTestId('hitl-answer-option-0-0').textContent).toBe('Staging');
    expect(screen.getByTestId('hitl-answer-option-0-1').textContent).toBe('Production');

    // Nothing chosen yet: submitting an empty answer would resume the run with
    // "the user did not answer", so the control is not offered until it is.
    expect(screen.getByTestId('hitl-answer-submit').hasAttribute('disabled')).toBe(true);

    fireEvent.click(screen.getByTestId('hitl-answer-option-0-0'));
    fireEvent.click(screen.getByTestId('hitl-answer-submit'));

    expect(onHitlResume).toHaveBeenCalledTimes(1);
    const payload = onHitlResume.mock.calls[0]?.[0] as HitlResumePayload;
    expect(payload.action).toBe('answer');
    // The tool call id is what routes the resume back to the parked call.
    expect(payload.toolCallId).toBe('call_mock_ask_user_1');
    expect(JSON.parse(payload.value ?? '')).toEqual({ environment: 'Staging' });
  });

  it('prefers a typed answer over the picked option for an allow_other question', () => {
    const onHitlResume = vi.fn();
    renderCard(ASK_USER_INTERRUPT, onHitlResume);

    fireEvent.click(screen.getByTestId('hitl-answer-option-0-1'));
    fireEvent.change(screen.getByTestId('hitl-answer-other-0'), { target: { value: 'sandbox-7' } });
    fireEvent.click(screen.getByTestId('hitl-answer-submit'));

    const answered = onHitlResume.mock.calls[0]?.[0] as HitlResumePayload;
    expect(JSON.parse(answered.value ?? '')).toEqual({
      environment: 'sandbox-7',
    });
  });

  it('answers a multi-select question with an ARRAY of the picked labels', () => {
    // `format_answer_map` accepts an array ONLY for a multi-select question and
    // refuses one anywhere else, so the shape is decided by the question.
    const onHitlResume = vi.fn();
    renderCard(
      {
        ...ASK_USER_INTERRUPT,
        questions: [
          {
            id: 'traits',
            question: 'Which traits matter?',
            options: [{ label: 'Safe' }, { label: 'Fast' }, { label: 'Cheap' }],
            multiSelect: true,
          },
        ],
      },
      onHitlResume,
    );

    fireEvent.click(screen.getByTestId('hitl-answer-option-0-0'));
    fireEvent.click(screen.getByTestId('hitl-answer-option-0-1'));
    fireEvent.click(screen.getByTestId('hitl-answer-submit'));

    const answered = onHitlResume.mock.calls[0]?.[0] as HitlResumePayload;
    expect(JSON.parse(answered.value ?? '')).toEqual({
      traits: ['Safe', 'Fast'],
    });
  });

  it('offers a free-text answer when the pause reached the browser with no questions', () => {
    // The regression this whole file is about is a pause the user cannot
    // answer. A dropped `questions` array must not reproduce it, so the
    // fallback answers with the bare JSON string the route also admits.
    const onHitlResume = vi.fn();
    renderCard({ ...ASK_USER_INTERRUPT, questions: [] }, onHitlResume);

    expect(screen.getByText('Which environment should I target?')).toBeTruthy();
    fireEvent.change(screen.getByTestId('hitl-answer-other-0'), { target: { value: 'Staging' } });
    fireEvent.click(screen.getByTestId('hitl-answer-submit'));

    const answered = onHitlResume.mock.calls[0]?.[0] as HitlResumePayload;
    expect(JSON.parse(answered.value ?? '')).toBe('Staging');
  });

  // #940 A8 — the stepped multi-question flow (ELITEA-2790/2791/2792).

  /** Three questions: one required select, one optional free-text, one required select. */
  const THREE_QUESTION_INTERRUPT: HitlInterrupt = {
    ...ASK_USER_INTERRUPT,
    questions: [
      { id: 'scope', question: 'Which scope?', options: [{ label: 'Project' }], allow_other: false },
      { id: 'notes', question: 'Anything else?', optional: true },
      { id: 'tone', question: 'Which tone?', options: [{ label: 'Formal' }], allow_other: false },
    ],
  };

  it('ELITEA-2791: a single-question flow shows Submit only — no progress, no Back/Next', () => {
    renderCard(ASK_USER_INTERRUPT, vi.fn());

    expect(screen.getByTestId('hitl-answer-submit')).toBeTruthy();
    // The three controls the case says must NOT be there. Asserted as absent
    // rather than as hidden: a rendered-but-invisible Next is still a control
    // a keyboard user reaches.
    expect(screen.queryByTestId('hitl-answer-progress')).toBeNull();
    expect(screen.queryByTestId('hitl-answer-back')).toBeNull();
    expect(screen.queryByTestId('hitl-answer-next')).toBeNull();
  });

  it('ELITEA-2790: a multi-question flow steps, keeps answers on Back, and submits once at the end', () => {
    const onHitlResume = vi.fn();
    renderCard(THREE_QUESTION_INTERRUPT, onHitlResume);

    // Step 1 of 3: Next, no Back, no Submit — and Next is disabled until the
    // required question is answered.
    expect(screen.getByTestId('hitl-answer-progress').textContent).toBe('Question 1 of 3');
    expect(screen.queryByTestId('hitl-answer-back')).toBeNull();
    expect(screen.queryByTestId('hitl-answer-submit')).toBeNull();
    expect(screen.getByTestId('hitl-answer-next').hasAttribute('disabled')).toBe(true);

    fireEvent.click(screen.getByTestId('hitl-answer-option-0-0'));
    expect(screen.getByTestId('hitl-answer-next').hasAttribute('disabled')).toBe(false);
    fireEvent.click(screen.getByTestId('hitl-answer-next'));

    // Step 2 of 3: Back appears.
    expect(screen.getByTestId('hitl-answer-progress').textContent).toBe('Question 2 of 3');
    expect(screen.getByTestId('hitl-answer-back')).toBeTruthy();

    // Back re-populates what was already chosen. This is the half that makes
    // stepping safe: state that is dropped on navigation silently loses an
    // answer the user believes they gave.
    fireEvent.click(screen.getByTestId('hitl-answer-back'));
    expect(screen.getByTestId('hitl-answer-progress').textContent).toBe('Question 1 of 3');
    expect(screen.getByTestId('hitl-answer-option-0-0').className).toContain('MuiButton-contained');

    fireEvent.click(screen.getByTestId('hitl-answer-next'));
    fireEvent.click(screen.getByTestId('hitl-answer-next'));

    // Step 3 of 3: Submit replaces Next.
    expect(screen.getByTestId('hitl-answer-progress').textContent).toBe('Question 3 of 3');
    expect(screen.queryByTestId('hitl-answer-next')).toBeNull();
    fireEvent.click(screen.getByTestId('hitl-answer-option-2-0'));
    fireEvent.click(screen.getByTestId('hitl-answer-submit'));

    // ONE resume, carrying every step's answer — not one per question.
    expect(onHitlResume).toHaveBeenCalledTimes(1);
    const payload = onHitlResume.mock.calls[0]?.[0] as HitlResumePayload;
    expect(JSON.parse(payload.value ?? '')).toEqual({ scope: 'Project', tone: 'Formal' });
  });

  it('ELITEA-2792: an unanswered OPTIONAL question is marked and does not block Next', () => {
    renderCard(THREE_QUESTION_INTERRUPT, vi.fn());

    fireEvent.click(screen.getByTestId('hitl-answer-option-0-0'));
    fireEvent.click(screen.getByTestId('hitl-answer-next'));

    // Marked optional on screen, and Next stays enabled with nothing entered.
    expect(screen.getByTestId('hitl-answer-optional-1')).toBeTruthy();
    expect(screen.getByTestId('hitl-answer-next').hasAttribute('disabled')).toBe(false);
  });

  it('leaves the approve/reject card alone for a pause that is not a clarification', () => {
    const onHitlResume = vi.fn();
    renderCard({ message: 'Approve?', available_actions: ['approve', 'reject'] }, onHitlResume);

    expect(screen.queryByTestId('hitl-answer-submit')).toBeNull();
    expect(screen.getByRole('button', { name: 'Approve' })).toBeTruthy();
  });
});
