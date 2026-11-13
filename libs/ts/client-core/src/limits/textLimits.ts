/**
 * The entity text limits of issue 1968, the same numbers the server enforces
 * (services/elitea-main/pkg/entitylimits) and the contract declares
 * (`maxLength` in api/openapi/v2.yaml): a name is at most 128 characters and a
 * description at most 2000. Chat message bodies, agent instructions and
 * welcome messages are NOT limited by these.
 *
 * Counted in characters the way the server counts runes: one per Unicode code
 * point, so an emoji is 1 (not the 2 of `String.length`) and a client never
 * refuses text the server would take, or the other way round.
 */
export const NAME_MAX_LENGTH = 128;
export const DESCRIPTION_MAX_LENGTH = 2000;

/** The number of characters (code points) in `value`, as the server counts them. */
export function characterCount(value: string): number {
  let count = 0;
  // for..of walks code points; it needs no array copy for a long value.
  for (const _ of value) count += 1;
  return count;
}

/** How a text compares with its limit, for a counter and for the submit gate. */
export interface TextLimitState {
  /** Characters in the value. */
  count: number;
  /** Characters left before the limit; 0 at the limit and past it. */
  remaining: number;
  /** Characters past the limit; 0 at or below it. */
  over: number;
  /** True when the value is longer than the limit: the form must not submit. */
  exceeded: boolean;
}

export function textLimitState(value: string, max: number): TextLimitState {
  const count = characterCount(value);
  return {
    count,
    remaining: Math.max(max - count, 0),
    over: Math.max(count - max, 0),
    exceeded: count > max,
  };
}

/** True when `value` is longer than `max` characters. */
export function exceedsLimit(value: string | null | undefined, max: number): boolean {
  return value != null && characterCount(value) > max;
}

/**
 * Cuts text a client DERIVES itself (a conversation title taken from a first
 * message) to `max` characters, never inside a surrogate pair. Text the user
 * typed is never cut: the form shows the counter and refuses the save.
 */
export function clipToLimit(value: string, max: number): string {
  if (characterCount(value) <= max) return value;
  return Array.from(value).slice(0, max).join('');
}
