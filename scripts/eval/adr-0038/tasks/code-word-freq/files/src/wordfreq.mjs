// Word frequency counting.

/**
 * @param {string} text
 * @param {number} [n]
 * @param {{ stopWords?: string[] }} [options]
 * @returns {[string, number][]}
 */
export function topWords(text, n, { stopWords = [] } = {}) {
  const stop = new Set(stopWords);
  const counts = {};
  for (const raw of text.split(/\W+/)) {
    if (!raw) continue;
    const word = raw.toLowerCase();
    if (stop.has(word)) continue;
    counts[word] = (counts[word] || 0) + 1;
  }
  const entries = Object.entries(counts).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  return n === undefined ? entries : entries.slice(0, n);
}
