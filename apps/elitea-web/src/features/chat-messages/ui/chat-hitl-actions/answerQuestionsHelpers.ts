/**
 * Pure helpers shared between `AnswerQuestionsControl` and `QuestionRow`.
 *
 * Split out of `AnswerQuestionsControl.tsx` to keep that file under the §3.5
 * 400-line budget once the multi-question stepping (#940 A8) landed beside
 * the single-card submit logic. See that file's own header for the feature
 * this supports.
 */
import type { HitlQuestion } from '../../lib/hitlInterrupts';

/** One question's answer: a list for multi-select, one entry otherwise. */
export type SelectionMap = Readonly<Record<string, readonly string[]>>;
export type TextMap = Readonly<Record<string, string>>;

/**
 * What a pause that carries NO questions is answered with.
 *
 * A pause can reach the browser with its `questions` dropped (an older stored
 * row, a path that never carried them). The user still owes the run an answer,
 * so one free-text row is offered and the submitted value is then a bare JSON
 * STRING rather than a per-question object — the shape
 * `AskUserRequest::format_answer` reads as "User answered: <text>" and the
 * continuation route admits alongside the object one.
 */
export const FALLBACK_QUESTIONS: readonly HitlQuestion[] = [{ id: 'q1', allow_other: true, options: [] }];

/**
 * The id one question's answer is keyed by.
 *
 * `q{n}` mirrors the runtime's own default (`AskUserQuestion::normalize`), so
 * a question that reached the browser without an id is still answerable rather
 * than silently dropped from the submitted object.
 */
export function questionId(question: HitlQuestion, index: number): string {
  return question.id && question.id.trim() ? question.id : `q${index + 1}`;
}

/** serde renames `multi_select` to `multiSelect`; the raw tool arguments use either. */
export function isMultiSelect(question: HitlQuestion): boolean {
  return question.multiSelect === true || question.multi_select === true;
}

/**
 * Whether this question may be left unanswered (ELITEA-2792).
 *
 * Absent reads as REQUIRED. The runtime defaults the flag to `false` for the
 * same reason (`AskUserQuestion::normalize`): a stored pause from before the
 * field existed carries no key, and treating that as optional would relax
 * every old clarification at once.
 */
export function isOptional(question: HitlQuestion): boolean {
  return question.optional === true;
}

/** Whether the question at `index` has an answer on screen right now. */
export function isAnswered(
  question: HitlQuestion,
  index: number,
  selected: SelectionMap,
  typed: TextMap,
): boolean {
  const id = questionId(question, index);
  return (selected[id] ?? []).length > 0 || (typed[id] ?? '').trim() !== '';
}

/** The label text of one option, or `''` for an option that carries none. */
export function optionLabel(option: { readonly label?: string } | undefined): string {
  return typeof option?.label === 'string' ? option.label : '';
}

/**
 * The answer object, built from what is on screen.
 *
 * A question with nothing chosen and nothing typed is OMITTED rather than sent
 * as an empty string: the runtime renders every answered question into the
 * substituted tool result, and an empty one would read to the model as an
 * answer the user did not give.
 */
export function buildAnswerValue(
  questions: readonly HitlQuestion[],
  selected: SelectionMap,
  typed: TextMap,
): Record<string, string | readonly string[]> {
  const answers: Record<string, string | readonly string[]> = {};
  questions.forEach((question, index) => {
    const id = questionId(question, index);
    const picked = selected[id] ?? [];
    const text = (typed[id] ?? '').trim();
    if (isMultiSelect(question)) {
      const values = text ? [...picked, text] : [...picked];
      if (values.length > 0) answers[id] = values;
      return;
    }
    const value = text || picked[0] || '';
    if (value) answers[id] = value;
  });
  return answers;
}
