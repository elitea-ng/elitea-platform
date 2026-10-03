/**
 * The end of a "#" or "@" picker query in the composer text (#6774).
 *
 * The key handler counts only the keys it sees, so its query is shorter than
 * the typed text after an IME composition or a key it does not record. Picking
 * an agent then removed `[anchor, anchor + query.length)` and left the rest of
 * the typed query (for example "Пров" after "#Пров") in the message.
 *
 * Read the range from the text itself. A picker query cannot hold whitespace (a
 * space or Enter ends it), so it runs from the trigger to the first whitespace
 * or the end of the text. When the trigger is no longer at the anchor, keep the
 * counted length.
 */
export function triggerQueryEnd(content: string, anchor: number, trigger: string, countedQuery: string): number {
  if (anchor < 0 || anchor >= content.length || content[anchor] !== trigger) {
    return Math.min(anchor + countedQuery.length, content.length);
  }
  const rest = content.slice(anchor + 1);
  const whitespace = /\s/u.exec(rest);
  return anchor + 1 + (whitespace === null ? rest.length : whitespace.index);
}
