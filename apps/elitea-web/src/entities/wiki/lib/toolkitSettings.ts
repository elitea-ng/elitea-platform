/**
 * Reading a repository identity out of a toolkit's stored settings.
 *
 * PORTED BUG-FOR-BUG from apps/deepwiki-ui/src/DeepWikiApp.jsx:163-470. Every
 * rule here decides which wikis a project can see, so a "tidier" rule is a
 * project whose wikis disappear or whose neighbour's appear. Where the legacy
 * logic looks arbitrary it is preserved and the reason is written down.
 */
import type { RepositoryIdentity, Toolkit, ToolkitSettings } from '../model/types';
import { artifactDisplayRepository, getArtifactSource } from './artifactSource';
import { parseRepositoryIdentity, extractAdoOrganization } from './repoUrl';
import { mergePlainObjects } from './wikiId';

/**
 * The first truthy string in a list of aliases.
 *
 * Extracted only to keep the alias chains below under the complexity limit.
 * `||` returns the first truthy operand and skips an empty string; this does
 * the same, so every call site keeps its original behaviour including that
 * detail — a settings field stored as "" falls through to the next alias
 * rather than winning as a configured-but-blank value.
 */
function firstAlias(
  source: Record<string, unknown>,
  ...names: readonly string[]
): string | null {
  for (const name of names) {
    const value = source[name];
    if (typeof value === 'string' && value) return value;
  }
  return null;
}

function getAdoConfig(settings: unknown): Record<string, unknown> {
  if (!settings || typeof settings !== 'object') return {};
  const s = settings as Record<string, unknown>;
  return mergePlainObjects(s.ado_configuration, s.toolkit_configuration_ado_configuration);
}

function getToolkitConfigPayload(toolkit: Toolkit | null | undefined): Record<string, unknown> {
  if (!toolkit || typeof toolkit !== 'object') return {};
  return mergePlainObjects(toolkit.toolkit_config, toolkit.configuration?.parameters);
}

function mergeToolkitIdentitySettings(
  settings: unknown,
  toolkitConfig: Record<string, unknown>,
): Record<string, unknown> {
  const merged = mergePlainObjects(toolkitConfig, settings);
  const adoConfig = mergePlainObjects(getAdoConfig(toolkitConfig), getAdoConfig(settings));
  if (Object.keys(adoConfig).length > 0) {
    merged.ado_configuration = adoConfig;
    merged.toolkit_configuration_ado_configuration = adoConfig;
  }
  return merged;
}

/**
 * The branch, under any of the eight names a settings screen may have stored
 * it. The ORDER is the precedence and is preserved: `active_branch` beats
 * `base_branch` beats `branch`, and the unprefixed name beats the
 * `toolkit_configuration_` one at each level.
 */
function getBranchFromSettings(settings: unknown): string | null {
  if (!settings || typeof settings !== 'object') return null;
  return firstAlias(
    settings as Record<string, unknown>,
    'active_branch',
    'toolkit_configuration_active_branch',
    'base_branch',
    'toolkit_configuration_base_branch',
    'github_base_branch',
    'toolkit_configuration_github_base_branch',
    'branch',
    'toolkit_configuration_branch',
  );
}

/**
 * `organization/project/repository` when all three are known, and the bare
 * repository otherwise.
 *
 * The fallback matters: an ADO toolkit with no project configured still
 * produces an identity, and it is the same one the legacy code produced, so a
 * wiki generated before the project was filled in stays findable.
 */
function buildAdoRepositoryIdentifier(settings: unknown): string | null {
  if (!settings || typeof settings !== 'object') return null;
  const s = settings as Record<string, unknown>;

  const adoConfig = getAdoConfig(s);
  const repository = firstAlias(
    s,
    'toolkit_configuration_repository_id',
    'repository_id',
    'repository',
    'repo',
  );
  const project =
    firstAlias(adoConfig, 'project') ?? firstAlias(s, 'toolkit_configuration_project', 'project');
  const organization =
    firstAlias(adoConfig, 'organization') ??
    firstAlias(s, 'organization') ??
    extractAdoOrganization(adoConfig);

  if (organization && project && repository) {
    return `${organization}/${project}/${repository}`;
  }

  return repository || null;
}

/** The repository, under any of the names a settings screen may have used. */
function getRepositoryFromSettings(settings: unknown): string | null {
  if (!settings || typeof settings !== 'object') return null;
  const s = settings as Record<string, unknown>;
  return (
    firstAlias(s, 'toolkit_configuration_github_repository', 'github_repository') ||
    buildAdoRepositoryIdentifier(s) ||
    firstAlias(s, 'repository', 'repo')
  );
}

/**
 * The id of a separate code toolkit this wiki toolkit points at.
 *
 * This is the whole subject of the legacy fix these helpers came from: a
 * DeepWiki toolkit may store a REFERENCE to a GitHub toolkit rather than a
 * repository, and reading the reference as if it were a repository name gave
 * "No repository configured" after a successful save.
 */
export function getCodeToolkitReference(settings: unknown): string | number | null {
  if (!settings || typeof settings !== 'object') return null;
  const s = settings as Record<string, unknown>;
  const alias = firstAlias(
    s,
    'toolkit_configuration_code_toolkit',
    'code_toolkit',
    'toolkit_configuration_code_repository',
    'code_repository',
  );
  if (alias !== null) return alias;
  // A reference may be stored as a NUMERIC toolkit id, which firstAlias skips
  // because it only accepts strings. The legacy chain returned it as-is, and a
  // caller resolves it either way.
  for (const name of [
    'toolkit_configuration_code_toolkit',
    'code_toolkit',
    'toolkit_configuration_code_repository',
    'code_repository',
  ]) {
    const value = s[name];
    if (typeof value === 'number' && value) return value;
  }
  return null;
}

/** The first non-null result of `read` over `sources`, in order. */
function firstFrom(
  sources: readonly unknown[],
  read: (source: unknown) => string | null,
): string | null {
  for (const source of sources) {
    const value = read(source);
    if (value) return value;
  }
  return null;
}

/**
 * The repository identity a toolkit is configured for.
 *
 * `resolvedRepoIdentity` wins when it resolves: it is what the caller got by
 * following a code-toolkit reference, and it is the only source that has seen
 * the referenced toolkit.
 */
export function getConfiguredRepoIdentity(
  toolkit: Toolkit | null | undefined,
  settings: ToolkitSettings | null | undefined,
  resolvedRepoIdentity: unknown = null,
): RepositoryIdentity | null {
  if (resolvedRepoIdentity) {
    const resolved = parseRepositoryIdentity(resolvedRepoIdentity);
    if (resolved.repository) return resolved;
  }

  const cfg = getToolkitConfigPayload(toolkit);
  const set = settings || toolkit?.settings || {};
  const merged = mergeToolkitIdentitySettings(set, cfg);
  // The three sources in precedence order: the merge first, then the raw
  // settings, then the toolkit's own config. Preserved from the legacy chain.
  const sources = [merged, set, cfg];

  // A FOLDER SOURCE IS READ FIRST, and it wins over every repository alias.
  // Two reasons, and the second is the one that bites: the facade settles a
  // body naming a folder on the folder (`folderInto` derives the repository
  // from it and overwrites whatever the body said), so the folder is the
  // source a generation would actually use; and the repository it derives is
  // `artifact://bucket/prefix`, which `parseRepositoryIdentity` reads as the
  // repository `artifact` on the branch `//bucket/prefix` — an identity that
  // matches no wiki this project owns.
  const folder = getArtifactSource(merged);
  if (folder) {
    return {
      repository: artifactDisplayRepository(folder),
      branch: firstFrom(sources, getBranchFromSettings),
    };
  }

  const repository = firstFrom(sources, getRepositoryFromSettings);
  const parsed = parseRepositoryIdentity(repository);
  if (!parsed.repository) return null;

  return {
    repository: parsed.repository,
    branch: firstFrom(sources, getBranchFromSettings) || parsed.branch || null,
  };
}

/**
 * The configured repository as a plain string.
 *
 * A thin wrapper over getConfiguredRepoIdentity, kept because it is what the
 * legacy call sites use and what the 27 oracle cases were written against.
 * Prefer getConfiguredRepoIdentity in new code: it carries the branch, and the
 * branch is what decides which wikis a project can see.
 */
export function getConfiguredRepo(
  toolkit: Toolkit | null | undefined,
  settings: ToolkitSettings | null | undefined,
  resolvedRepoName: unknown = null,
): string | null {
  const resolved = parseRepositoryIdentity(resolvedRepoName);
  if (resolved.repository) return resolved.repository;
  return getConfiguredRepoIdentity(toolkit, settings, null)?.repository || null;
}
