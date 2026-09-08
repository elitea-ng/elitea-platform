/**
 * Turning "the reader highlighted these words" into the range the canvas
 * create route takes.
 *
 * ── THE RANGE IS COUNTED IN BYTES, NOT IN CHARACTERS ──────────────────────
 * `POST /elitea_core/canvases/prompt_lib/{project}` slices the stored message
 * with `canvas_content_starts_at` / `canvas_content_ends_at`, and the server
 * does that slice on a Go string — which indexes BYTES. JavaScript's
 * `String.prototype.indexOf` and `.length` count UTF-16 code units. The two
 * agree for ASCII and disagree for everything else: `'é'` is one JS character
 * and two bytes, `'日'` is one character and three, an emoji outside the BMP is
 * two JS characters and four bytes.
 *
 * So a client that sent `stored.indexOf(selected)` would carve a range that
 * starts EARLIER than the reader's selection by one byte per non-ASCII
 * character before it, and the route would not refuse it: the offsets are
 * still inside the message, so the split succeeds and silently puts the wrong
 * half of the answer in the canvas. Worse, a slice that lands in the middle of
 * a multi-byte sequence stores a broken code point. That is why every offset
 * this module answers is measured with `TextEncoder`, and why the tests beside
 * it are written over multibyte text.
 *
 * ── WHAT A SELECTION CAN AND CANNOT LOCATE ────────────────────────────────
 * The reader selects RENDERED markdown; the server splits the STORED source.
 * The two are the same words but not always the same string — `**bold**` is
 * rendered as `bold`, a list marker is rendered as a bullet. So the selected
 * text is looked up in the stored source, and a selection that spans syntax
 * the renderer removed is simply NOT FOUND. Not found is answered as
 * `undefined` and the affordance stays disabled, because the alternative —
 * carving the nearest range that does match — would split the reader's answer
 * somewhere they did not choose, and the split is destructive (the create
 * deletes the text item it splits). The server takes the same position for the
 * same reason: it refuses an out-of-range selection rather than clamping it.
 *
 * A selection that occurs more than once resolves to the FIRST occurrence.
 * That is a real limitation and it is stated rather than hidden: the DOM gives
 * no offset into the source text, so there is nothing here to tell two
 * identical runs of words apart.
 */

/** One `TextEncoder`, not one per call — this runs on every selection change. */
const encoder = new TextEncoder();

/** How many BYTES `text` occupies once encoded as UTF-8. */
export function utf8ByteLength(text: string): number {
  return encoder.encode(text).length;
}

/** The half-open byte range the canvas create route splits a message on. */
export interface CanvasByteRange {
  readonly startsAt: number;
  readonly endsAt: number;
}

/**
 * Where `selected` sits inside `stored`, in BYTES.
 *
 * `undefined` for an empty selection and for one the stored text does not
 * contain — see the module doc for why neither is approximated.
 */
export function canvasByteRange(stored: string, selected: string): CanvasByteRange | undefined {
  const needle = selected.trim();
  if (needle === '' || stored === '') return undefined;
  const charIndex = stored.indexOf(needle);
  if (charIndex < 0) return undefined;
  const startsAt = utf8ByteLength(stored.slice(0, charIndex));
  return { startsAt, endsAt: startsAt + utf8ByteLength(needle) };
}

/**
 * The selected text inside `container`, or `undefined`.
 *
 * Pure over the two inputs so the rule can be tested without a live document:
 * a collapsed caret is not a selection, a selection whose range sits outside
 * this element belongs to a different message, and whitespace alone is not a
 * document worth carving out.
 *
 * `container.contains(range.commonAncestorContainer)` rather than
 * `selection.containsNode`: the latter is not implemented everywhere this app
 * is tested, and the ancestor check is the claim that actually matters — one
 * transcript holds many answers, and a selection dragged across two of them
 * must not offer to carve either.
 */
export function selectionTextWithin(container: HTMLElement | null | undefined, selection: Selection | null): string | undefined {
  if (!container || !selection || selection.isCollapsed || selection.rangeCount === 0) return undefined;
  const range = selection.getRangeAt(0);
  if (!container.contains(range.commonAncestorContainer)) return undefined;
  const text = selection.toString();
  return text.trim() === '' ? undefined : text;
}
