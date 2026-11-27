/**
 * What an import leaves for the person to do (issue 3137), read from the
 * document they imported and the server's answer:
 *  - the toolkits to connect: an import keeps a toolkit's non-secret settings
 *    only, so a toolkit that needed credentials lands unbound;
 *  - the memory seeds waiting for review (they import as pending entries);
 *  - the agents whose file configures a heartbeat schedule: the heartbeat
 *    itself is turned on per install, in the Heartbeat tab, after publishing.
 *
 * And the server's refusal of an invalid file (422 `import_invalid`), which
 * names the field it refused.
 */
import { EliteaApiError } from '@/shared/api/generated/mutator';

import type { EntityExportDocument } from '../api/lifecycleApi';

export interface ImportFollowUps {
  readonly toolkits: readonly { readonly name: string; readonly type: string }[];
  readonly memorySeeds: number;
  readonly heartbeatAgents: readonly string[];
}

type Rec = Record<string, unknown>;

/** A string field, or `''` for anything else. */
function text(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function records(value: unknown): Rec[] {
  return Array.isArray(value) ? value.filter((item): item is Rec => typeof item === 'object' && item !== null) : [];
}

/**
 * The server marks each toolkit whose credentials or configuration references
 * it dropped (`needs_connection: true`, ImportedToolkit), so a toolkit list in
 * the answer is trusted: only the marked toolkits are named. Only when the
 * server answered no list is every toolkit of the file named, because the
 * import never carries credentials.
 */
function toolkitsOf(document: EntityExportDocument, result: unknown): ImportFollowUps['toolkits'] {
  const listed = (result as Rec | null | undefined)?.['toolkits'];
  const source = Array.isArray(listed) ? records(listed).filter((toolkit) => toolkit['needs_connection'] === true) : records(document.toolkits);
  return source.map((toolkit) => ({ name: text(toolkit['name']), type: text(toolkit['type']) }));
}

function hasHeartbeat(entry: Rec): boolean {
  return records(entry['versions']).some((version) => {
    const proactivity = (version['meta'] as Rec | undefined)?.['proactivity'];
    return typeof proactivity === 'object' && proactivity !== null && (proactivity as Rec)['mode'] === 'heartbeat';
  });
}

export function importFollowUps(document: EntityExportDocument, result?: unknown): ImportFollowUps {
  const entries = records(document.applications);
  return {
    toolkits: toolkitsOf(document, result),
    memorySeeds: entries.reduce((sum, entry) => sum + records(entry['memory_seeds']).length, 0),
    heartbeatAgents: entries.filter(hasHeartbeat).map((entry) => text(entry['name'])),
  };
}

export interface ImportRefusal {
  readonly message: string;
  /** The path of the refused field, e.g. `applications[0].versions[0].meta.run_caps.max_tool_calls`. */
  readonly field: string | undefined;
}

/** The server's `import_invalid` refusal, else `undefined`. */
export function importRefusalOf(cause: unknown): ImportRefusal | undefined {
  if (!(cause instanceof EliteaApiError) || cause.failure.kind !== 'http') return undefined;
  const body = cause.failure.body as Rec | null | undefined;
  if (body?.['code'] !== 'import_invalid' || typeof body['error'] !== 'string') return undefined;
  return { message: body['error'], field: typeof body['field'] === 'string' && body['field'] !== '' ? body['field'] : undefined };
}
