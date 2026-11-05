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
  // True once the current record has any content (a delimiter, a quote or a
  // character); a record with no content at end of input is not emitted.
  let recordStarted = false;

  const endRecord = () => {
    row.push(field);
    rows.push(row);
    row = [];
    field = '';
    recordStarted = false;
  };

  let i = 0;
  while (i < text.length) {
    const c = text[i];
    if (inQuotes) {
      if (c === '"') {
        if (text[i + 1] === '"') {
          field += '"';
          i += 2;
          continue;
        }
        inQuotes = false;
      } else {
        field += c;
      }
      i++;
      continue;
    }

    if (c === '"' && field === '') {
      inQuotes = true;
      recordStarted = true;
      i++;
    } else if (c === delimiter) {
      row.push(field);
      field = '';
      recordStarted = true;
      i++;
    } else if (c === '\r' && text[i + 1] === '\n') {
      endRecord();
      i += 2;
    } else if (c === '\n') {
      endRecord();
      i++;
    } else {
      field += c;
      recordStarted = true;
      i++;
    }
  }

  if (inQuotes) throw new Error('unterminated quoted field');
  if (recordStarted) endRecord();
  return rows;
}

/**
 * Serialise records to CSV text.
 * @param {unknown[][]} rows
 * @param {{ delimiter?: string }} [options]
 * @returns {string}
 */
export function stringifyCSV(rows, { delimiter = ',' } = {}) {
  const quote = (value) => {
    const s = value === null || value === undefined ? '' : String(value);
    const needsQuotes =
      s.includes(delimiter) || s.includes('"') || s.includes('\r') || s.includes('\n');
    return needsQuotes ? `"${s.replaceAll('"', '""')}"` : s;
  };
  return rows.map((row) => row.map(quote).join(delimiter)).join('\r\n');
}
