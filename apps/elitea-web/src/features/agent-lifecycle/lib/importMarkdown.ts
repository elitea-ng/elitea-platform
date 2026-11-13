import { dump as dumpYaml } from 'js-yaml';

/**
 * The pure pieces of the `.agent.md` import (#1996): reading typed values out
 * of the parsed frontmatter and assembling the import entry from it. Split out
 * of `importDocument.ts` for the §3.5 400-line and cyclomatic-complexity (12)
 * budgets. Nothing here throws; the refusals stay in `importDocument.ts`.
 */

export type Front = Record<string, unknown>;

export function asRecord(value: unknown): Front {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Front) : {};
}

export function defined(entries: Front): Front {
  return Object.fromEntries(Object.entries(entries).filter(([, value]) => value !== undefined && value !== null));
}

/** A string value, else `fallback` (an empty string is kept). */
function str(value: unknown, fallback = ''): string {
  return typeof value === 'string' ? value : fallback;
}

/** A non-empty string value, else `fallback`. */
function nonEmptyStr(value: unknown, fallback: string): string {
  return typeof value === 'string' && value !== '' ? value : fallback;
}

function arr(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

export function newKey(prefix: string): string {
  return `${prefix}-${globalThis.crypto.randomUUID()}`;
}

function asTags(value: unknown): { name: string; data?: unknown }[] {
  if (!Array.isArray(value)) return [];
  const tags: { name: string; data?: unknown }[] = [];
  for (const item of value) {
    if (typeof item === 'string' && item.trim() !== '') {
      tags.push({ name: item.trim() });
    } else {
      const record = asRecord(item);
      if (typeof record['name'] === 'string' && record['name'] !== '') {
        tags.push({ name: record['name'], ...(record['data'] === undefined ? {} : { data: record['data'] }) });
      }
    }
  }
  return tags;
}

/** The trimmed `name`, or an empty string when it is absent or not a string. */
export function frontName(front: Front): string {
  return typeof front['name'] === 'string' ? front['name'].trim() : '';
}

export function frontAgentType(front: Front): string {
  return str(front['agent_type'], 'agent');
}

/** A pipeline's `state`/`nodes`/`entry_point`/interrupt keys, lifted back into its YAML instructions. */
export function pipelineInstructions(front: Front): string {
  return dumpYaml(
    defined({
      state: front['state'],
      entry_point: front['entry_point'],
      interrupt_after: front['interrupt_after'],
      interrupt_before: front['interrupt_before'],
      nodes: front['nodes'],
    }),
  );
}

export function frontVersion(front: Front): string {
  return nonEmptyStr(front['version'], 'base');
}

interface ToolkitBlocks {
  readonly toolkits: Front[];
  readonly tools: { import_uuid: string }[];
  readonly skipped: string[];
}

export function collectToolkits(front: Front, agentName: string): ToolkitBlocks {
  const toolkits: Front[] = [];
  const tools: { import_uuid: string }[] = [];
  const skipped: string[] = [];
  for (const raw of arr(front['toolkits'])) {
    const block = asRecord(raw);
    const toolkitName = str(block['toolkit']);
    const type = nonEmptyStr(block['type'], 'custom');
    if (type === 'application') {
      // A nested agent is exported as a reference with no settings; there is
      // nothing in this file to rebuild it from.
      skipped.push(`${agentName}: ${toolkitName || type} (nested agent)`);
      continue;
    }
    const importUuid = newKey('tool');
    const settings = { ...asRecord(block['settings']) };
    if (Array.isArray(block['tools']) && block['tools'].length > 0) settings['selected_tools'] = block['tools'];
    toolkits.push(
      defined({ entity: 'toolkits', import_uuid: importUuid, name: toolkitName, type, settings, meta: block['meta'] }),
    );
    tools.push({ import_uuid: importUuid });
  }
  return { toolkits, tools, skipped };
}

interface SkillBlocks {
  readonly skills: Front[];
  readonly skillRefs: { import_uuid: string; version_name: string }[];
  readonly skipped: string[];
}

export function collectSkills(front: Front, agentName: string): SkillBlocks {
  const skills: Front[] = [];
  const skillRefs: { import_uuid: string; version_name: string }[] = [];
  const skipped: string[] = [];
  for (const raw of arr(front['skills'])) {
    const block = asRecord(raw);
    const skillName = str(block['name']);
    if (skillName === '') {
      skipped.push(`${agentName}: skill without a name`);
      continue;
    }
    const skillVersion = nonEmptyStr(block['version'], 'base');
    const importUuid = newKey('skill');
    skills.push({
      entity: 'skills',
      import_uuid: importUuid,
      name: skillName,
      description: str(block['description']),
      versions: [{ name: skillVersion, instructions: str(block['instructions']) }],
    });
    skillRefs.push({ import_uuid: importUuid, version_name: skillVersion });
  }
  return { skills, skillRefs, skipped };
}

export interface EntryParts {
  readonly front: Front;
  readonly name: string;
  readonly agentType: string;
  readonly instructions: string;
  readonly version: string;
  readonly uuid: string;
  readonly tools: { import_uuid: string }[];
  readonly skillRefs: { import_uuid: string; version_name: string }[];
}

export function buildEntry({
  front,
  name,
  agentType,
  instructions,
  version,
  uuid,
  tools,
  skillRefs,
}: EntryParts): Front {
  return {
    entity: agentType === 'pipeline' ? 'pipelines' : 'agents',
    name,
    description: str(front['description']),
    import_uuid: uuid,
    versions: [
      {
        name: version,
        import_version_uuid: `${uuid}-${version}`,
        instructions,
        agent_type: agentType,
        llm_settings: defined({
          model_name: front['model'],
          temperature: front['temperature'],
          max_tokens: front['max_tokens'],
          top_p: front['top_p'],
        }),
        meta: defined({ step_limit: front['step_limit'], internal_tools: front['internal_tools'] }),
        tools,
        skills: skillRefs,
        variables: arr(front['variables']),
        conversation_starters: arr(front['conversation_starters']),
        welcome_message: str(front['welcome_message']),
        tags: asTags(front['tags']),
        ...soulField(front),
      },
    ],
  };
}

function soulField(front: Front): { soul?: unknown } {
  const soul = front['soul'];
  return typeof soul === 'object' && soul !== null ? { soul } : {};
}
