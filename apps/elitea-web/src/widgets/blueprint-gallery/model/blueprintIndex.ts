/**
 * The gallery's index (issue 3137): `<base>/blueprints/index.json`, which the
 * build writes from the repository's `blueprints/` tree
 * (`scripts/blueprints/build.mjs`, served by the `elitea-blueprints` Vite
 * plugin). Read defensively: an entry missing its name or file is left out
 * rather than shown as a card that cannot import.
 */
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import { fetchStaticFile } from '@/shared/lib/download';

const BLUEPRINT_INDEX_SCHEMA = 'elitea.blueprints/v1';

export interface BlueprintAvatar {
  /** A drawn default avatar's name (`owl`), when the blueprint uses one. */
  readonly defaultName?: string | undefined;
  /** The still image's path under `blueprints/`, when it has uploaded media. */
  readonly still?: string | undefined;
}

interface BlueprintRequirement {
  readonly type: string;
  readonly label: string;
}

export interface BlueprintEntry {
  readonly kind: 'agent' | 'team';
  readonly slug: string;
  readonly name: string;
  readonly description: string;
  /** The importable file under `blueprints/`: `.agent.md`, `.agent.zip` or `.team.zip`. */
  readonly file: string;
  readonly avatar: BlueprintAvatar;
  readonly showcases: readonly string[];
  readonly requirements: readonly BlueprintRequirement[];
  readonly brief: string;
  readonly surface: 'cloud' | 'desktop';
  /** A team's members, lead first when it has one. */
  readonly members: readonly { readonly slug: string; readonly name: string; readonly avatar: BlueprintAvatar }[];
}

interface BlueprintIndex {
  readonly agents: readonly BlueprintEntry[];
  readonly teams: readonly BlueprintEntry[];
}

type Rec = Record<string, unknown>;

function rec(value: unknown): Rec {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Rec) : {};
}

function str(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function avatarOf(value: unknown): BlueprintAvatar {
  const raw = rec(value);
  return { defaultName: str(raw['default']) || undefined, still: str(raw['still']) || undefined };
}

function requirementsOf(value: unknown): BlueprintRequirement[] {
  return (Array.isArray(value) ? value : []).map(rec).map((item) => ({ type: str(item['type']), label: str(item['label']) })).filter((item) => item.label !== '');
}

function membersOf(raw: Rec): BlueprintEntry['members'] {
  const members = (Array.isArray(raw['members']) ? raw['members'] : []).map(rec).map((item) => ({ slug: str(item['slug']), name: str(item['name']), avatar: avatarOf(item['avatar']) }));
  const lead = str(raw['lead']);
  return [...members.filter((member) => member.slug === lead), ...members.filter((member) => member.slug !== lead)];
}

function entryOf(kind: BlueprintEntry['kind'], value: unknown): BlueprintEntry | undefined {
  const raw = rec(value);
  const name = str(raw['name']);
  const file = str(raw['file']);
  if (name === '' || file === '' || file.includes('..')) return undefined;
  return {
    kind,
    slug: str(raw['slug']) || name,
    name,
    description: str(raw['description']),
    file,
    avatar: avatarOf(raw['avatar']),
    showcases: (Array.isArray(raw['showcases']) ? raw['showcases'] : []).filter((item): item is string => typeof item === 'string'),
    requirements: requirementsOf(raw['requirements']),
    brief: str(raw['brief']),
    surface: raw['surface'] === 'desktop' ? 'desktop' : 'cloud',
    members: kind === 'team' ? membersOf(raw) : [],
  };
}

function entriesOf(kind: BlueprintEntry['kind'], value: unknown): BlueprintEntry[] {
  return (Array.isArray(value) ? value : []).map((item) => entryOf(kind, item)).filter((item): item is BlueprintEntry => item !== undefined);
}

/** The index as the gallery shows it; another schema reads as empty, never as a guess. */
export function readBlueprintIndex(value: unknown): BlueprintIndex {
  const raw = rec(value);
  if (raw['schema'] !== BLUEPRINT_INDEX_SCHEMA) return { agents: [], teams: [] };
  return { agents: entriesOf('agent', raw['agents']), teams: entriesOf('team', raw['teams']) };
}

/** A path under `blueprints/` as the app serves it. */
export function blueprintPath(path: string): string {
  return `blueprints/${path}`;
}

/** Fetched only while the gallery is open: the index is a build file, so it never goes stale within a session. */
export function useBlueprintIndex(enabled: boolean): UseQueryResult<BlueprintIndex> {
  return useQuery({
    queryKey: ['blueprints', 'index'],
    enabled,
    staleTime: Infinity,
    queryFn: async ({ signal }) => readBlueprintIndex(JSON.parse(await (await fetchStaticFile(blueprintPath('index.json'), signal)).text())),
  });
}
