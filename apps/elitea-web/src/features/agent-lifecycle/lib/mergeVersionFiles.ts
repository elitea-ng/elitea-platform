/**
 * Folding the per-version files of a zip into one agent entry per agent
 * (#2098), and assembling the import document from parsed files.
 */
import type { ParsedMarkdown, ReadImportResult } from './importTypes';

type Rec = Record<string, unknown>;

function versionsOf(entry: Rec): Rec[] {
  return Array.isArray(entry['versions']) ? (entry['versions'] as Rec[]) : [];
}

function versionName(version: Rec | undefined): string {
  return typeof version?.['name'] === 'string' ? version['name'] : '';
}

/** A zip holds one file per version, so files that share this key are versions of ONE agent (#2098). */
function agentKey(entry: Rec): string {
  return `${String(entry['entity'])}\u0000${String(entry['name'])}`;
}

/** A toolkit or skill block minus its per-file import key: the same block in two files is ONE toolkit, not two. */
function blockKey(block: Rec): string {
  const { import_uuid: _key, ...rest } = block;
  return JSON.stringify(rest);
}

function remapRefs(refs: unknown, replacements: ReadonlyMap<string, string>): unknown {
  if (!Array.isArray(refs)) return refs;
  return refs.map((ref: Rec) => {
    const key = String(ref['import_uuid']);
    return { ...ref, import_uuid: replacements.get(key) ?? key };
  });
}

/**
 * Folds the per-version files of a zip into one agent entry per agent.
 *
 * Without this every file became its own application, so an agent exported
 * with three versions imported as three agents (#2098). Files merge when they
 * share the entity kind and name AND the agent does not already hold a
 * version of that name; a repeat is a different agent of the same name and
 * stays apart. The `base` version goes first, because the first version of an
 * entry is the one the import makes the default. Toolkits and skills that are
 * identical across the merged files collapse to one, and the later files'
 * references are re-pointed at it.
 */
function mergeVersionFiles(parsed: readonly ParsedMarkdown[]): ParsedMarkdown[] {
  const merged: ParsedMarkdown[] = [];
  // Per merged entry, so two agents that only share a name never share a toolkit.
  const knownBlocks: Map<string, string>[] = [];
  for (const file of parsed) {
    const key = agentKey(file.entry);
    const incoming = versionName(versionsOf(file.entry)[0]);
    const index = merged.findIndex(
      (candidate) =>
        agentKey(candidate.entry) === key && !versionsOf(candidate.entry).some((v) => versionName(v) === incoming),
    );
    const known = knownBlocks[index] ?? new Map<string, string>();
    const replacements = new Map<string, string>();
    const keep = (blocks: Rec[]): Rec[] =>
      blocks.filter((block) => {
        const seen = known.get(blockKey(block));
        if (seen === undefined) {
          known.set(blockKey(block), String(block['import_uuid']));
          return true;
        }
        replacements.set(String(block['import_uuid']), seen);
        return false;
      });
    const toolkits = keep(file.toolkits);
    const skills = keep(file.skills);
    // Every version of the file is re-pointed at the toolkit or skill that was kept, on both paths.
    const added = versionsOf(file.entry).map((version) => ({
      ...version,
      tools: remapRefs(version['tools'], replacements),
      skills: remapRefs(version['skills'], replacements),
    }));
    const target = merged[index];
    if (target === undefined) {
      merged.push({ ...file, entry: { ...file.entry, versions: added }, toolkits, skills });
      knownBlocks.push(known);
      continue;
    }
    const versions = [...versionsOf(target.entry), ...added].sort(
      (a, b) => Number(versionName(b) === 'base') - Number(versionName(a) === 'base'),
    );
    merged[index] = {
      entry: { ...target.entry, versions },
      toolkits: [...target.toolkits, ...toolkits],
      skills: [...target.skills, ...skills],
      skipped: [...target.skipped, ...file.skipped],
    };
  }
  return merged;
}

export function assemble(files: ParsedMarkdown[]): ReadImportResult {
  const parsed = mergeVersionFiles(files);
  return {
    document: {
      ok: true,
      applications: parsed.map((p) => p.entry),
      toolkits: parsed.flatMap((p) => p.toolkits),
      skills: parsed.flatMap((p) => p.skills),
    },
    skipped: parsed.flatMap((p) => p.skipped),
  };
}
