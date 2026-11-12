/**
 * The entity text limits of issue 1968 for the web and desktop UI: a name is at
 * most 128 characters and a description at most 2000, the numbers the server
 * enforces and the contract declares. They come from `@elitea/client-core`, the
 * one home the mobile app reads too.
 *
 * A form that uses them does not cut what the person typed or pasted: it shows
 * `CharacterCounter` (turns to "N characters over the limit" past the limit)
 * and refuses the save with `exceedsLimit`. Only text the app derives itself
 * (a conversation title taken from a first message) is cut, with `clipToLimit`.
 */
import { textLimitState } from '@elitea/client-core/limits';

export {
  DESCRIPTION_MAX_LENGTH,
  NAME_MAX_LENGTH,
  clipToLimit,
  exceedsLimit,
  textLimitState,
} from '@elitea/client-core/limits';

/** A counter that stays out of the way until the last fifth of the limit is used. */
export function isNearLimit(value: string, max: number): boolean {
  return textLimitState(value, max).remaining <= Math.ceil(max / 5);
}
