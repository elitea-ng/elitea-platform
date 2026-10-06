/** Source pin: consumer-contract2. These values identify content; they grant no access. */
export interface CodeDebugArtifactReference {
  readonly schema_version: 'elitea.runtime.code-debug-artifact.v1';
  readonly project_id: number;
  readonly bucket: 'code-debug';
  readonly name: string;
  readonly media_type: 'application/json';
  readonly byte_length: number;
  readonly sha256: string;
}

export interface CodeDebugProof {
  readonly revision: 1;
  readonly original_visit: { readonly visit_id: string; readonly revision: 1; readonly digest_sha256: string };
  readonly attempt: number;
  readonly execution_id: string;
  readonly generation: string;
  readonly node_id: string;
  readonly activation_id: string;
  readonly request_sha256: string;
  readonly status: 'committed' | 'denied' | 'unavailable';
  readonly artifact?: CodeDebugArtifactReference;
}

export const MAX_CODE_DEBUG_BYTES = 3 * 1024 * 1024;
const digestPattern = /^[0-9a-f]{64}$/;
const encoder = new TextEncoder();

function record(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? value as Record<string, unknown> : undefined;
}

function exactKeys(value: Record<string, unknown>, required: readonly string[], optional: readonly string[] = []): boolean {
  return required.every(key => Object.hasOwn(value, key))
    && Object.keys(value).every(key => required.includes(key) || optional.includes(key));
}

function digest(value: unknown): value is string {
  return typeof value === 'string' && value.length === 64 && digestPattern.test(value);
}

function nonzeroDigest(value: unknown): value is string {
  return digest(value) && value !== '0'.repeat(64);
}

function positiveInteger(value: unknown, maximum: number): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 1 && value <= maximum;
}

function executionIdentity(value: unknown): value is string {
  return typeof value === 'string' && value !== '' && encoder.encode(value).length <= 256
    && !/[\p{Cc}\uD800-\uDFFF]/u.test(value);
}

function generation(value: unknown): value is string {
  if (typeof value !== 'string' || !/^[1-9][0-9]{0,19}$/.test(value)) return false;
  return BigInt(value) <= 18446744073709551615n && BigInt(value).toString() === value;
}

function parseVisit(value: unknown): CodeDebugProof['original_visit'] | undefined {
  const row = record(value);
  if (!row || !exactKeys(row, ['visit_id', 'revision', 'digest_sha256'])) return undefined;
  if (row['revision'] !== 1 || !nonzeroDigest(row['visit_id']) || !nonzeroDigest(row['digest_sha256'])) return undefined;
  return { visit_id: row['visit_id'], revision: 1, digest_sha256: row['digest_sha256'] };
}

export function parseCodeDebugArtifactReference(value: unknown): CodeDebugArtifactReference | undefined {
  const row = record(value);
  if (!row || !exactKeys(row, ['schema_version', 'project_id', 'bucket', 'name', 'media_type', 'byte_length', 'sha256'])) return undefined;
  if (row['schema_version'] !== 'elitea.runtime.code-debug-artifact.v1' || row['bucket'] !== 'code-debug' || row['media_type'] !== 'application/json') return undefined;
  if (!positiveInteger(row['project_id'], 2147483647) || !positiveInteger(row['byte_length'], MAX_CODE_DEBUG_BYTES) || !digest(row['sha256'])) return undefined;
  if (typeof row['name'] !== 'string' || row['name'].length !== 69 || !/^[0-9a-f]{64}\.json$/.test(row['name'])) return undefined;
  return {
    schema_version: 'elitea.runtime.code-debug-artifact.v1', project_id: row['project_id'], bucket: 'code-debug',
    name: row['name'], media_type: 'application/json', byte_length: row['byte_length'], sha256: row['sha256'],
  };
}

function boundedProof(value: unknown): Record<string, unknown> | undefined {
  const row = record(value);
  if (!row) return undefined;
  try {
    if (encoder.encode(JSON.stringify(row)).length > 4096) return undefined;
  } catch {
    return undefined;
  }
  return exactKeys(row, ['revision', 'original_visit', 'attempt', 'execution_id', 'generation', 'node_id', 'activation_id', 'request_sha256', 'status'], ['artifact']) ? row : undefined;
}

function nodeIdentity(value: unknown): value is string {
  return typeof value === 'string' && value.length >= 1 && value.length <= 128 && !/[^A-Za-z0-9_.:-]/.test(value);
}

function validIdentity(row: Record<string, unknown>): boolean {
  return row['revision'] === 1 && positiveInteger(row['attempt'], 16) && executionIdentity(row['execution_id'])
    && generation(row['generation']) && nodeIdentity(row['node_id'])
    && digest(row['activation_id']) && digest(row['request_sha256']);
}

/** Parse the frozen public DTO. Never infer actor rights from these selectors. */
export function parseCodeDebugProof(value: unknown): CodeDebugProof | undefined {
  const row = boundedProof(value);
  if (!row || !validIdentity(row)) return undefined;
  const visit = parseVisit(row['original_visit']);
  const status = row['status'];
  if (!visit || (status !== 'committed' && status !== 'denied' && status !== 'unavailable')) return undefined;
  const artifact = parseCodeDebugArtifactReference(row['artifact']);
  if (status === 'committed' ? !artifact : Object.hasOwn(row, 'artifact')) return undefined;
  return {
    revision: 1, original_visit: visit, attempt: row['attempt'] as number,
    execution_id: row['execution_id'] as string, generation: row['generation'] as string, node_id: row['node_id'] as string,
    activation_id: row['activation_id'] as string, request_sha256: row['request_sha256'] as string, status,
    ...(artifact ? { artifact } : {}),
  };
}

function matchesNode(metadata: Record<string, unknown> | undefined, proof: CodeDebugProof): boolean {
  return metadata !== undefined && metadata['langgraph_node'] === proof.node_id && metadata['original_name'] === proof.node_id && metadata['node_type'] === 'code';
}

function hasCodeDebugProof(metadata: Record<string, unknown> | undefined): boolean {
  return metadata !== undefined && Object.hasOwn(metadata, 'code_debug_v1');
}

function matchingCodeDebugProof(metadata: Record<string, unknown> | undefined, alternate: Record<string, unknown> | undefined): CodeDebugProof | undefined {
  const proof = parseCodeDebugProof(metadata?.['code_debug_v1']);
  const other = parseCodeDebugProof(alternate?.['code_debug_v1']);
  if (!proof || !other || JSON.stringify(proof) !== JSON.stringify(other)
    || !matchesNode(metadata, proof) || !matchesNode(alternate, proof)) return undefined;
  return proof;
}

/** Absence stays absent. Invalid or conflicting metadata becomes an inert warning. */
export function codeDebugToolMeta(attrs: unknown): Record<string, unknown> {
  const row = record(attrs);
  const metadata = record(row?.['metadata']);
  const alternate = record(record(row?.['tool_meta'])?.['metadata']);
  if (!hasCodeDebugProof(metadata) && !hasCodeDebugProof(alternate)) return {};
  return { code_debug_v1: matchingCodeDebugProof(metadata, alternate) ?? null };
}
