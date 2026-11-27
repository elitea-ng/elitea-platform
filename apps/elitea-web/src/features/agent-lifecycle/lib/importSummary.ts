/**
 * What the Import dialog shows before the write (issues 3137 and 3142): the
 * team, each agent with its avatar, lead and model, the counts of what comes
 * with them, and what the person has to do afterwards or should know first.
 */
import type { EntityExportDocument } from '../api/lifecycleApi';
import { importFollowUps } from './importFollowUps';
import type { PendingAvatar } from './importTypes';
import type { ImportTeam } from './teamManifest';

/** One agent of the file, as the preview lists it. */
export interface ImportSummaryAgent {
  /** The entry's `import_uuid`, else its position: unique within the file. */
  readonly key: string;
  readonly name: string;
  readonly description: string;
  readonly lead: boolean;
  /** The model the file names; `undefined` when it names none (a blueprint). */
  readonly model: string | undefined;
  /** The first version's `import_version_uuid`: the key of its avatar files in a zip. */
  readonly versionKey: string | undefined;
  /** An icon the entry already carries (`meta.icon_meta.url`), e.g. a JSON export's. */
  readonly iconUrl: string | undefined;
  /** The first version's `avatar_pack`, as the file carries it. */
  readonly pack: object | undefined;
}

export interface ImportDocumentSummary {
  readonly names: readonly string[];
  readonly agents: readonly ImportSummaryAgent[];
  /** Set when the file is a team zip: the team its agents form. */
  readonly team?: { readonly name: string; readonly description: string; readonly lead: string | undefined } | undefined;
  readonly toolkitCount: number;
  readonly skillCount: number;
  readonly datasetCount: number;
  readonly memorySeedCount: number;
  /** The agents whose file configures a heartbeat schedule: it is turned on after publishing. */
  readonly heartbeatAgents: readonly string[];
  /** Parts of the file the import cannot carry; shown, not dropped silently. */
  readonly skipped: readonly string[];
}

type Rec = Record<string, unknown>;

function record(value: unknown): Rec {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Rec) : {};
}

function records(value: unknown): Rec[] {
  return Array.isArray(value) ? value.filter((item): item is Rec => typeof item === 'object' && item !== null) : [];
}

function text(value: unknown): string | undefined {
  return typeof value === 'string' && value.trim() !== '' ? value.trim() : undefined;
}

function firstVersion(entry: Rec): Rec {
  return records(entry['versions'])[0] ?? {};
}

function summaryAgent(entry: Rec, index: number, leadKey: string | undefined): ImportSummaryAgent {
  const version = firstVersion(entry);
  const key = text(entry['import_uuid']) ?? `#${String(index + 1)}`;
  const pack = version['avatar_pack'];
  return {
    key,
    name: text(entry['name']) ?? `#${String(index + 1)}`,
    description: text(entry['description']) ?? '',
    lead: leadKey !== undefined && key === leadKey,
    model: text(record(version['llm_settings'])['model_name']),
    versionKey: text(version['import_version_uuid']),
    iconUrl: text(record(record(version['meta'])['icon_meta'])['url']) ?? text(record(record(entry['meta'])['icon_meta'])['url']),
    pack: typeof pack === 'object' && pack !== null ? pack : undefined,
  };
}

/** What the confirmation dialog shows before the user commits to the import. */
export function summarizeImportDocument(
  document: EntityExportDocument,
  skipped: readonly string[] = [],
  team?: ImportTeam,
): ImportDocumentSummary {
  const entries = records(document.applications);
  const followUps = importFollowUps(document);
  const agents = (document.applications ?? []).map((entry, index) => summaryAgent(record(entry), index, team?.lead));
  return {
    names: agents.map((agent) => agent.name),
    agents,
    toolkitCount: document.toolkits?.length ?? 0,
    skillCount: document.skills?.length ?? 0,
    datasetCount: entries.reduce((sum, entry) => sum + records(record(entry['evaluation'])['datasets']).length, 0),
    memorySeedCount: followUps.memorySeeds,
    heartbeatAgents: followUps.heartbeatAgents,
    skipped,
    ...(team === undefined ? {} : { team: { name: team.name, description: team.description, lead: team.leadName } }),
  };
}

const IMAGE_FILE = /\.(png|jpe?g|webp|svg)$/i;

/** The image a zip carries for an agent's still (else its first image slot), if any: the preview's face. */
export function pendingStillOf(avatars: readonly PendingAvatar[] | undefined, versionKey: string | undefined): PendingAvatar | undefined {
  if (avatars === undefined || versionKey === undefined) return undefined;
  const images = avatars.filter((avatar) => avatar.versionKey === versionKey && IMAGE_FILE.test(avatar.path));
  return images.find((avatar) => avatar.slot === 'still') ?? images[0];
}
