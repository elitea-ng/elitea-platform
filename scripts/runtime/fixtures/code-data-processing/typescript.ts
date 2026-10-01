import { gzipSync, gunzipSync, strToU8, strFromU8 } from 'npm:fflate@0.8.2';
import { z } from 'npm:zod@3.24.2';

type Row = [number, number, number, number];
type Metric = {stage: string; processing_ms: number; raw_bytes: number; compressed_bytes: number};
interface Data {payload: string; rows: number; seed: number; totals: number[]; metrics: Metric[]; [key: string]: unknown}
const rowSchema = z.tuple([z.number().int().nonnegative(), z.number().int().min(0).max(4),
  z.number().int().positive(), z.union([z.literal(-1), z.literal(0), z.literal(1)])]);
class Ledger {
  readonly totals = Array<number>(5).fill(0);
  accepted = 0;
  rejected = 0;
  refunds = 0;
  weighted = 0;
  append(row: Row): void {
    const [id, category, cents, status] = rowSchema.parse(row);
    this.totals[category] += cents * status;
    this.weighted += (id + 1) * cents * status;
    if (status === 0) this.rejected++;
    else { this.accepted++; if (status < 0) this.refunds++; }
  }
  async reconcile(rows: Row[]): Promise<void> {
    for (let first = 0; first < rows.length; first += 500) {
      for (const row of rows.slice(first, first + 500)) this.append(row);
      await Promise.resolve();
    }
  }
}
function encode(value: Uint8Array): string {
  let text = '';
  for (let i = 0; i < value.length; i += 8192) text += String.fromCharCode(...value.subarray(i, i + 8192));
  return btoa(text);
}
export default async function process(state: {javascript_data: Data}) {
  const started = performance.now();
  const data = state.javascript_data;
  const raw = gunzipSync(Uint8Array.from(atob(data.payload), c => c.charCodeAt(0)));
  const rows: Row[] = JSON.parse(strFromU8(raw));
  const ledger = new Ledger();
  await ledger.reconcile(rows);
  if (JSON.stringify(ledger.totals) !== JSON.stringify(data.totals)) throw new Error('Cross-language totals disagree');
  const sha = Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', raw)), b => b.toString(16).padStart(2, '0')).join('');
  const compressed = gzipSync(strToU8(JSON.stringify(rows)), {level: 6});
  return {...data, payload: encode(compressed), accepted: ledger.accepted, rejected: ledger.rejected,
    refunds: ledger.refunds, weighted: ledger.weighted, sha256: sha,
    metrics: [...data.metrics, {stage: 'typescript', processing_ms: performance.now() - started,
      raw_bytes: raw.length, compressed_bytes: compressed.length}]};
}
