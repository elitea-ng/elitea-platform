/**
 * Validating the DeepWiki toolkit settings before they are saved.
 *
 * WHY VALIDATION LIVES HERE AND NOT IN THE COMPONENT. The legacy screen parsed
 * the JSON inside its save handler and reported failures through a string
 * (`setSettingsError(\`Invalid JSON: ${err.message}\`)`), which means the only
 * way to test the rules was to drive the form. They are the rules that decide
 * whether a generation can find its repository at all, so they are a function.
 *
 * THE PARSE IS NOT THE ONLY CHECK, and that is the substance of this file. The
 * legacy code accepted any valid JSON object — including one with no repository
 * — and the failure surfaced much later, as a generation that ran and produced
 * nothing. A settings screen that accepts a configuration the feature cannot
 * use is worse than one that refuses it.
 *
 * TWO KINDS OF FINDING. `problems` block Save; `hints` do not. The model
 * settings are hints because a document without them is legal and may well
 * work — see ENGINE_FALLBACK_* below for what it costs when it does not.
 */
import {
  getCodeToolkitReference,
  getConfiguredRepoIdentity,
  readArtifactSource,
  type ToolkitSettings,
} from '@/entities/wiki';

/**
 * The models the DeepWiki engine asks the platform gateway for when the
 * toolkit names none.
 *
 * MEASURED 2026-09-02 (PR #725). The gateway resolves a model PER PROJECT, so
 * a project with no row for these names answers
 * `404 model is not configured for this project`, and the generation
 * "completes" with no pages. Nothing on this screen said so: the only place
 * the refusal appeared was the gateway's own log.
 */
const ENGINE_FALLBACK_CHAT_MODEL = 'gpt-4o-mini';
const ENGINE_FALLBACK_EMBEDDING_MODEL = 'text-embedding-3-large';

/** What is wrong with a draft, in the operator's terms. */
export interface SettingsProblem {
  /** The field to attach the message to, or null for the document as a whole. */
  readonly field: string | null;
  readonly message: string;
}

/**
 * A setting whose ABSENCE the engine papers over with a hardcoded default.
 *
 * Separate from `SettingsProblem` on purpose: a hint never blocks Save. The
 * saved document is legal and the legacy screen accepted it, and a toolkit
 * whose project does resolve the fallback model works exactly as before — so
 * refusing it here would break a working configuration to warn about a
 * broken one. What it must not do is stay silent.
 */
export interface SettingsHint {
  /** The settings key that is absent. */
  readonly field: string;
  /** The model the engine asks the platform for instead. */
  readonly fallback: string;
}

export interface ParsedSettings {
  readonly settings: ToolkitSettings | null;
  readonly problems: readonly SettingsProblem[];
  readonly hints: readonly SettingsHint[];
}

/**
 * Parse and check a settings draft.
 *
 * Returns every problem rather than the first: an operator fixing a
 * configuration one message at a time, re-saving between each, is the
 * experience this avoids.
 */
export function parseSettingsDraft(draft: string): ParsedSettings {
  const trimmed = draft.trim();
  if (trimmed === '') {
    // An empty document is not the same as `{}`. Saving it would silently
    // clear a configuration the operator did not mean to touch.
    return {
      settings: null,
      problems: [{ field: null, message: 'Settings cannot be empty. Use {} to clear them.' }],
      hints: [],
    };
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch (error) {
    return {
      settings: null,
      problems: [
        {
          field: null,
          message: `Not valid JSON: ${error instanceof Error ? error.message : 'parse failed'}`,
        },
      ],
      hints: [],
    };
  }

  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return {
      settings: null,
      problems: [{ field: null, message: 'Settings must be a JSON object.' }],
      hints: [],
    };
  }

  const settings = parsed as ToolkitSettings;
  return { settings, problems: sourceProblems(settings), hints: modelHints(settings) };
}

/**
 * What is wrong with the SOURCE the document names.
 *
 * A WIKI HAS ONE SOURCE, and it may be a folder rather than a repository. The
 * three rules are one rule seen from three sides, and they are mutually
 * exclusive by construction: a folder that does not parse is reported as a
 * folder and not as a missing repository, and a document naming both sources
 * is reported once.
 */
function sourceProblems(settings: ToolkitSettings): SettingsProblem[] {
  const problems: SettingsProblem[] = [];
  const folder = readArtifactSource(settings);
  if (folder.status === 'invalid') {
    problems.push({ field: folder.field, message: folder.message });
  }
  if (folder.status !== 'absent' && namesRepositorySource(settings)) {
    problems.push({ field: ARTIFACT_SOURCE_FIELD, message: TWO_SOURCES });
  }
  // The one check the legacy screen did not make. Without a resolvable
  // repository a generation runs and finds nothing, and the operator learns
  // that minutes later from an empty wiki.
  if (folder.status === 'absent' && !getConfiguredRepoIdentity(null, settings, null)?.repository) {
    problems.push({
      field: 'repository',
      message:
        'No repository is configured. Set one of github_repository, repository, repo, ' +
        'or an Azure DevOps organization/project/repository_id.',
    });
  }
  return problems;
}

/** The canonical toolkit parameter naming a folder source. */
const ARTIFACT_SOURCE_FIELD = 'artifact_configuration';

/**
 * WHY THIS COMBINATION IS A PROBLEM AND NOT A PRECEDENCE.
 *
 * The facade refuses a body naming both a `code_toolkit` and an
 * `artifact_configuration` with a 400 ("a wiki has one source"), and it
 * OVERWRITES a repository named beside a folder with the one it derives. So a
 * document holding both either cannot start a generation, or starts one from
 * material the operator did not choose. A form that saved it would move that
 * discovery to the generation that ran and produced the wrong wiki.
 */
const TWO_SOURCES =
  'The settings name both a repository source and an artifact folder. A wiki has one ' +
  'source: remove code_toolkit and the repository fields, or remove artifact_configuration.';

/**
 * Does the document name a repository source BESIDE the folder?
 *
 * The identity resolver prefers a folder, so the repository half is invisible
 * to it while a folder is present. The folder keys are stripped to ask the
 * question the resolver would have answered without them; the suffix match
 * covers the `toolkit_configuration_` alias without a second copy of the
 * entity's key list.
 */
function namesRepositorySource(settings: ToolkitSettings): boolean {
  if (getCodeToolkitReference(settings) !== null) return true;
  const withoutFolder = Object.fromEntries(
    Object.entries(settings).filter(([key]) => !key.endsWith(ARTIFACT_SOURCE_FIELD)),
  );
  return Boolean(getConfiguredRepoIdentity(null, withoutFolder, null)?.repository);
}

/**
 * Whether a settings key holds a usable model name, under either of the two
 * names a settings screen may have stored it (`entities/wiki`'s alias rule:
 * the unprefixed name and its `toolkit_configuration_` twin), and treating an
 * empty string as absent the way the identity resolver does.
 */
function hasModelName(settings: ToolkitSettings, field: string): boolean {
  const source = settings as Record<string, unknown>;
  for (const name of [field, `toolkit_configuration_${field}`]) {
    const value = source[name];
    if (typeof value === 'string' && value.trim() !== '') return true;
  }
  return false;
}

/** One hint per model setting the engine would have to substitute a default for. */
function modelHints(settings: ToolkitSettings): SettingsHint[] {
  const hints: SettingsHint[] = [];
  if (!hasModelName(settings, 'llm_model')) {
    hints.push({ field: 'llm_model', fallback: ENGINE_FALLBACK_CHAT_MODEL });
  }
  if (!hasModelName(settings, 'embedding_model')) {
    hints.push({ field: 'embedding_model', fallback: ENGINE_FALLBACK_EMBEDDING_MODEL });
  }
  return hints;
}

/**
 * Every key that names a REPOSITORY source, under each spelling the entity's
 * identity and reference readers accept.
 *
 * The list is here rather than in the entity because it exists for one job:
 * writing a folder source into a draft that already named a repository. It is
 * derived from `getCodeToolkitReference` and `getRepositoryFromSettings` —
 * the ADO organization and project are NOT in it, because neither yields an
 * identity without one of the four repository names.
 */
const REPOSITORY_SOURCE_KEYS = [
  'code_toolkit',
  'toolkit_configuration_code_toolkit',
  'code_repository',
  'toolkit_configuration_code_repository',
  'github_repository',
  'toolkit_configuration_github_repository',
  'repository',
  'repo',
  'repository_id',
  'toolkit_configuration_repository_id',
];

/**
 * The settings with their source replaced: a folder, or no folder at all.
 *
 * WRITING A FOLDER REMOVES THE REPOSITORY KEYS. The two cannot both stand —
 * see TWO_SOURCES — so a picker that added the folder and left the repository
 * would produce exactly the document the next rule refuses, and the operator
 * would have to finish the job by hand in the JSON. Removing them is visible:
 * the draft the editor shows IS the document that will be saved.
 *
 * The prefix is written AS TYPED, only trimmed. The reader canonicalises
 * `docs/` to `docs` on its own, and rewriting the field under the operator
 * would take the slash back out of the box while they are still typing a path.
 */
export function withArtifactSource(
  settings: ToolkitSettings,
  source: { readonly bucket: string; readonly prefix: string } | null,
): ToolkitSettings {
  const next: Record<string, unknown> = { ...settings };
  delete next['toolkit_configuration_artifact_configuration'];
  if (source === null) {
    delete next[ARTIFACT_SOURCE_FIELD];
    return next;
  }
  for (const key of REPOSITORY_SOURCE_KEYS) delete next[key];
  next[ARTIFACT_SOURCE_FIELD] = { bucket: source.bucket.trim(), prefix: source.prefix.trim() };
  return next;
}

/** A draft is savable when it parses and has no problems. */
export function canSaveSettings(parsed: ParsedSettings): boolean {
  return parsed.settings !== null && parsed.problems.length === 0;
}
