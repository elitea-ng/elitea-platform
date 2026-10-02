import { parse } from 'npm:csv-parse@5.6.0/sync';
import { gzipSync, gunzipSync, strToU8, strFromU8 } from 'npm:fflate@0.8.2';
import { z } from 'npm:zod@3.24.2';

const rowSchema = z.tuple([z.number().int().nonnegative(), z.number().int().min(0).max(4),
  z.number().int().positive(), z.union([z.literal(-1), z.literal(0), z.literal(1)])]);
function decodeBase64(value) { return Uint8Array.from(atob(value), c => c.charCodeAt(0)); }
function encodeBase64(value) {
  let text = '';
  for (let i = 0; i < value.length; i += 8192) text += String.fromCharCode(...value.subarray(i, i + 8192));
  return btoa(text);
}
async function parseTransactions(payload) {
  await Promise.resolve();
  return parse(strFromU8(gunzipSync(decodeBase64(payload))), {columns: true})
    .map(row => rowSchema.parse([Number(row.id), Number(row.category), Number(row.cents), Number(row.status)]));
}
function aggregate(rows) {
  const groups = Array(5).fill(0);
  for (const [, category, cents, status] of rows) groups[category] += cents * status;
  return groups;
}
export default async function process(state) {
  const started = performance.now();
  const data = state.python_data;
  const rows = await parseTransactions(data.payload);
  rows.sort((a, b) => a[0] - b[0]);
  if (rows.length !== data.rows || rows.some((row, i) => row[0] !== i)) throw new Error('Record identities changed');
  const raw = strToU8(JSON.stringify(rows));
  const compressed = gzipSync(raw, {level: 6});
  return {...data, payload: encodeBase64(compressed), totals: aggregate(rows),
    metrics: [...data.metrics, {stage: 'javascript', processing_ms: performance.now() - started,
      raw_bytes: raw.length, compressed_bytes: compressed.length}]};
}
