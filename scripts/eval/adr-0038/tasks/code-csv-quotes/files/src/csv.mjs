// Minimal CSV reader/writer (RFC 4180 flavoured).

/**
 * Parse CSV text into an array of records (arrays of strings).
 * @param {string} text
 * @param {{ delimiter?: string }} [options]
 * @returns {string[][]}
 */
export function parseCSV(text, { delimiter = ',' } = {}) {
  const rows = [];
  let row = [];
  let field = '';
  let inQuotes = false;

  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (inQuotes) {
      if (c === '"') {
        inQuotes = false;
      } else {
        field += c;
      }
    } else if (c === '"') {
      inQuotes = true;
    } else if (c === delimiter) {
      row.push(field);
      field = '';
    } else if (c === '\n') {
      row.push(field);
      rows.push(row);
      row = [];
      field = '';
    } else {
      field += c;
    }
  }

  if (field !== '') {
    row.push(field);
    rows.push(row);
  }
  return rows;
}

/**
 * Serialise records to CSV text.
 * @param {unknown[][]} rows
 * @param {{ delimiter?: string }} [options]
 * @returns {string}
 */
export function stringifyCSV(rows, { delimiter = ',' } = {}) {
  return rows
    .map((row) =>
      row
        .map((value) => {
          const s = String(value);
          return s.includes(delimiter) ? `"${s}"` : s;
        })
        .join(delimiter),
    )
    .join('\n');
}
