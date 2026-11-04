// Word frequency counting.

const WORD_RE = /[\p{L}\p{M}\p{N}]+(?:['’-][\p{L}\p{M}\p{N}]+)*/gu;

const normalize = (word) => word.normalize('NFC').toLowerCase().replace(/’/g, "'");

/**
 * @param {string} text
 * @param {number} [n]
 * @param {{ stopWords?: string[] }} [options]
 * @returns {[string, number][]}
 */
export function topWords(text, n, { stopWords = [] } = {}) {
  const stop = new Set(stopWords.map(normalize));
  // Map preserves first-insertion order, which is first appearance.
  const counts = new Map();
  for (const [raw] of text.normalize('NFC').matchAll(WORD_RE)) {
    const word = normalize(raw);
    if (stop.has(word)) continue;
    counts.set(word, (counts.get(word) ?? 0) + 1);
  }
  // Array.prototype.sort is stable, so ties keep first-appearance order.
  const entries = [...counts].sort((a, b) => b[1] - a[1]);
  return n === undefined ? entries : entries.slice(0, n);
}
