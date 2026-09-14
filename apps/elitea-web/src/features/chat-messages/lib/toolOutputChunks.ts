/** Versioned UTF-8 chunk contract from libs/proto/elitea/runtime/v1/node_event.proto. */
interface ToolOutputChunk {
  readonly offset_bytes: number;
  readonly total_bytes: number;
  readonly sha256: string;
  readonly final: boolean;
}

function parseChunk(value: unknown): ToolOutputChunk | undefined {
  if (!value || typeof value !== 'object') return undefined;
  const row = value as Record<string, unknown>;
  if (typeof row['offset_bytes'] !== 'number' || !Number.isSafeInteger(row['offset_bytes']) || row['offset_bytes'] < 0 ||
      typeof row['total_bytes'] !== 'number' || !Number.isSafeInteger(row['total_bytes']) || row['total_bytes'] <= 0 || row['total_bytes'] > 1048576 ||
      typeof row['sha256'] !== 'string' || !/^[0-9a-f]{64}$/.test(row['sha256']) || typeof row['final'] !== 'boolean') return undefined;
  return row as unknown as ToolOutputChunk;
}

/** Main verifies the final digest before publishing; the browser enforces offsets and replay identity. */
export function appendToolOutputChunk(previous: unknown, output: unknown, metadata: unknown, priorMetadata: unknown): { output: string; chunk: ToolOutputChunk } | undefined {
  const chunk = parseChunk(metadata);
  if (!chunk || typeof output !== 'string') return undefined;
  const encoder = new TextEncoder();
  const data = encoder.encode(output);
  const old = typeof previous === 'string' ? previous : '';
  const bytes = encoder.encode(old);
  if (!data.length || data.length > 8192 || chunk.offset_bytes + data.length > chunk.total_bytes || chunk.final !== (chunk.offset_bytes + data.length === chunk.total_bytes)) return undefined;
  const prior = parseChunk(priorMetadata);
  if (prior ? prior.sha256 !== chunk.sha256 || prior.total_bytes !== chunk.total_bytes : chunk.offset_bytes !== 0 || bytes.length !== 0) return undefined;
  if (chunk.offset_bytes < bytes.length) {
    if (chunk.offset_bytes + data.length > bytes.length || !data.every((byte, index) => byte === bytes[chunk.offset_bytes + index])) return undefined;
    return prior ? { output: old, chunk: prior } : undefined;
  }
  if (chunk.offset_bytes !== bytes.length) return undefined;
  return { output: old + output, chunk };
}
