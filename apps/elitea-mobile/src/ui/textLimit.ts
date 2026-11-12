import { DESCRIPTION_MAX_LENGTH, NAME_MAX_LENGTH, exceedsLimit, textLimitState } from '@elitea/client-core/limits';

/**
 * The entity text limits of client contract 1.51 (issue 1968): a name is at most 128 characters, a
 * description at most 2000 (`@elitea/client-core/limits`, the numbers the server enforces). A form
 * never cuts what the person typed: `TextField` shows `limitMessage` under the field and the form
 * blocks Save with `exceedsLimit`. The characters are counted like the server counts runes.
 */
export { DESCRIPTION_MAX_LENGTH, NAME_MAX_LENGTH, exceedsLimit };

export interface LimitMessage {
  /** "N characters left", from the last fifth of the limit; null before. */
  note: string | null;
  /** "N characters over the limit…", once the text is longer than the limit; null otherwise. */
  error: string | null;
}

export function limitMessage(value: string, max: number): LimitMessage {
  const { remaining, over, exceeded } = textLimitState(value, max);
  if (exceeded) return { note: null, error: `${over} ${over === 1 ? 'character' : 'characters'} over the limit. Shorten the text to save.` };
  if (remaining <= Math.ceil(max / 5)) return { note: `${remaining} ${remaining === 1 ? 'character' : 'characters'} left`, error: null };
  return { note: null, error: null };
}
