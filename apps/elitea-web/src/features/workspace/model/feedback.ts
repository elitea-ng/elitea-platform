/**
 * The person's feedback to the agent (Decline on a plan card, "Decline and
 * tell the agent why" on an approval): bounded in the box as the desktop
 * host bounds it, in UTF-8 bytes (`MAX_FEEDBACK_BYTES` in
 * libs/rust/local-tools), so the box never holds text the host would drop.
 */

/** What the host keeps of the person's text: 2 KiB of UTF-8. */
export const MAX_FEEDBACK_BYTES = 2048;

const encoder = new TextEncoder();

/** `text` cut to at most `maxBytes` bytes of UTF-8, never inside a character. */
export function clipUtf8(text: string, maxBytes: number = MAX_FEEDBACK_BYTES): string {
  if (encoder.encode(text).length <= maxBytes) return text;
  let used = 0;
  let end = 0;
  for (const char of text) {
    used += encoder.encode(char).length;
    if (used > maxBytes) break;
    end += char.length;
  }
  return text.slice(0, end);
}
