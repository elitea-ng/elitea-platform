/**
 * The `team.yaml` of a team zip (issue 3010): a set of `.agent.md` files
 * imported as one saved team (ADR-0050 decision 8).
 *
 *     schema: elitea.team/v1      # optional (issue 3137); another value is refused
 *     kind: team                  # optional
 *     name: Dev team
 *     description: ...            # optional
 *     floor_policy: lead          # optional: addressed | lead
 *     members:                    # 1-16, roster order, paths beside team.yaml
 *       - agents/dev-coordinator.agent.md
 *     lead: agents/dev-coordinator.agent.md   # optional, one of the members
 */
import { load as loadYaml } from 'js-yaml';
import type JSZip from 'jszip';

import { t } from '@/shared/i18n';

import { TEAM_FILE_SCHEMA, checkFileSchema } from './fileSchema';
import { ImportDocumentError } from './importError';
import { asRecord } from './importMarkdown';

/**
 * The team a team zip describes (issue 3010): its `team.yaml`, with each
 * member file resolved to the `import_uuid` of the agent entry it became.
 */
export interface ImportTeam {
  readonly name: string;
  readonly description: string;
  readonly floor_policy?: string | undefined;
  /** The lead member's import_uuid. */
  readonly lead?: string | undefined;
  /** The members' import_uuids, in roster order. */
  readonly members: readonly string[];
  /** For the preview: the lead's agent name. */
  readonly leadName?: string | undefined;
}

/** A team has at most this many agents (agentteams.MaxMembers on the server). */
const MAX_TEAM_MEMBERS = 16;
const TEAM_MANIFEST = /(?:^|\/)team\.ya?ml$/i;

/** The shallowest `team.yaml` (or `.yml`) of the zip, if any. */
export function findTeamManifest(zip: JSZip): string | undefined {
  return Object.keys(zip.files)
    .filter((entry) => !(zip.files[entry]?.dir ?? true) && TEAM_MANIFEST.test(entry) && !entry.startsWith('__MACOSX/'))
    .sort((a, b) => a.split('/').length - b.split('/').length || a.localeCompare(b))[0];
}

/** `path` read relative to the folder of the file `base` (a zip root for an empty `base`), with `.` and `..` resolved. */
export function resolveZipPath(manifest: string, path: string): string {
  const parts = manifest.split('/').slice(0, -1);
  for (const part of path.trim().split('/')) {
    if (part === '' || part === '.') continue;
    if (part === '..') parts.pop();
    else parts.push(part);
  }
  return parts.join('/');
}

interface TeamManifest {
  readonly name: string;
  readonly description: string;
  readonly floorPolicy: string | undefined;
  readonly lead: string | undefined;
  readonly members: readonly string[];
}

function optionalString(value: unknown): string | undefined {
  return typeof value === 'string' && value.trim() !== '' ? value.trim() : undefined;
}

function manifestRecord(text: string, file: string): Record<string, unknown> {
  let front: Record<string, unknown>;
  try {
    front = asRecord(loadYaml(text));
  } catch {
    throw new ImportDocumentError(t('features.agentLifecycle.import.team.badYaml', '{{file}} is not valid YAML.', { file }));
  }
  checkFileSchema(front, TEAM_FILE_SCHEMA);
  const kind = optionalString(front['kind']);
  if (kind !== undefined && kind !== 'team') {
    throw new ImportDocumentError(t('features.agentLifecycle.import.team.badKind', '{{file}} must have kind: team.', { file }));
  }
  return front;
}

function manifestMembers(front: Record<string, unknown>, file: string): string[] {
  const raw = front['members'];
  const members = Array.isArray(raw) ? raw.filter((m): m is string => typeof m === 'string' && m.trim() !== '') : [];
  if (members.length === 0 || !Array.isArray(raw) || members.length !== raw.length) {
    throw new ImportDocumentError(
      t('features.agentLifecycle.import.team.noMembers', '{{file}} must list its members as the paths of their .agent.md files.', { file }),
    );
  }
  if (members.length > MAX_TEAM_MEMBERS) {
    throw new ImportDocumentError(
      t('features.agentLifecycle.import.team.tooManyMembers', 'A team has at most {{limit}} agents.', { limit: MAX_TEAM_MEMBERS }),
    );
  }
  const resolved = members.map((member) => resolveZipPath(file, member));
  if (new Set(resolved).size !== resolved.length) {
    throw new ImportDocumentError(t('features.agentLifecycle.import.team.repeatedMember', '{{file}} lists a member twice.', { file }));
  }
  return resolved;
}

/** Reads `team.yaml`: `kind: team`, `name`, `description`, `members` (paths), `lead` (a member path), `floor_policy`. */
export function parseTeamManifest(text: string, file: string): TeamManifest {
  const front = manifestRecord(text, file);
  const name = optionalString(front['name']);
  if (name === undefined) {
    throw new ImportDocumentError(t('features.agentLifecycle.import.team.noName', '{{file}} has no team name.', { file }));
  }
  const members = manifestMembers(front, file);
  const leadPath = optionalString(front['lead']);
  const lead = leadPath === undefined ? undefined : resolveZipPath(file, leadPath);
  if (lead !== undefined && !members.includes(lead)) {
    throw new ImportDocumentError(
      t('features.agentLifecycle.import.team.leadNotMember', 'The lead {{lead}} is not one of the members of {{file}}.', {
        lead: leadPath,
        file,
      }),
    );
  }
  return { name, description: optionalString(front['description']) ?? '', floorPolicy: optionalString(front['floor_policy']), lead, members };
}
