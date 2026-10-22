/**
 * Reading the toolkit a wiki belongs to, and the repository it is configured for.
 *
 * THE TOOLKIT IS THE WIKI'S ADDRESS. The legacy bundle took `project_id` and
 * `toolkit_id` out of its URL (`/app/ui_host/deepwiki/ui/{project}/{toolkit}`,
 * DeepWikiApp.jsx:756-788) and fetched
 * `/elitea_core/tool/prompt_lib/{project}/{toolkit}` to get its settings. A
 * project can hold more than one wiki toolkit, each pointed at a different
 * repository, so "the project's wikis" is not a thing you can ask for without
 * naming which toolkit.
 *
 * TWO REQUESTS, NOT ONE, and the second is conditional. `code_toolkit` in the
 * settings is an INTEGER referring to another toolkit — the github/gitlab/ado
 * one that actually holds the repository. The repository identity comes from
 * there when it is set, and from the wiki toolkit's own settings when it is
 * not. Following that reference is what `getConfiguredRepoIdentity`'s third
 * argument is for, and skipping it produces a wiki list filtered against the
 * wrong repository — which renders as "you have no wikis".
 *
 * eliteaFetch RETURNS THE ENVELOPE, not the body (#132). Every read here goes
 * through `unwrapBody`.
 */
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';

import { getCodeToolkitReference, getConfiguredRepoIdentity } from '../lib/toolkitSettings';
import type { RepositoryIdentity, Toolkit, ToolkitSettings } from '../model/types';

/**
 * One toolkit row, as `GET /elitea_core/tool/prompt_lib/{project}/{toolkit}`
 * returns it.
 *
 * Not exported: it is reached through `WikiToolkitContext`, and an exported
 * type with no importer is the dead-code shape this repository has removed six
 * times (#126/#129/#134/#136/#138/#149).
 */
interface WikiToolkit extends Toolkit {
  readonly id?: string | number;
  readonly name?: string;
  readonly type?: string;
}

/** The wiki toolkit, its settings, and the repository it is configured for. */
export interface WikiToolkitContext {
  readonly toolkit: WikiToolkit;
  readonly settings: ToolkitSettings;
  readonly identity: RepositoryIdentity | null;
}

function toolkitUrl(projectId: string, toolkitId: string): string {
  return `/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`;
}

/** `settings`, then `toolkit_config` — the order the legacy bundle read them in. */
function settingsOf(toolkit: WikiToolkit): ToolkitSettings {
  if (toolkit.settings && typeof toolkit.settings === 'object') return toolkit.settings;
  if (toolkit.toolkit_config && typeof toolkit.toolkit_config === 'object') {
    return toolkit.toolkit_config;
  }
  return {};
}

export async function fetchWikiToolkit(
  projectId: string,
  toolkitId: string,
): Promise<WikiToolkitContext> {
  const toolkit = (unwrapBody(await eliteaFetch<unknown>(toolkitUrl(projectId, toolkitId))) ??
    {}) as WikiToolkit;
  const settings = settingsOf(toolkit);

  // The code toolkit, when the wiki names one. A reference that cannot be
  // followed is NOT fatal: the identity falls back to the wiki toolkit's own
  // settings, which is what the legacy bundle did when the fetch failed
  // (DeepWikiApp.jsx:793-810 warns and returns null).
  let referenced: RepositoryIdentity | null = null;
  const reference = getCodeToolkitReference(settings);
  if (reference !== null && reference !== '') {
    try {
      const code = (unwrapBody(
        await eliteaFetch<unknown>(toolkitUrl(projectId, String(reference))),
      ) ?? {}) as WikiToolkit;
      referenced = getConfiguredRepoIdentity(code, settingsOf(code), null);
    } catch {
      referenced = null;
    }
  }

  return { toolkit, settings, identity: getConfiguredRepoIdentity(toolkit, settings, referenced) };
}

/**
 * One query-key namespace, declared once — the read/write key-namespace split
 * that made saved data look absent in #132.
 *
 * ROOTED AT `deepwiki`, not `wiki`. `useSaveWikiSettings` and `useDeleteWiki`
 * invalidate `['deepwiki']` after a write; a toolkit key rooted anywhere else
 * is exactly the split #132 describes — the save succeeds, the settings screen
 * keeps showing the previous values, and nothing reports it.
 */
const wikiToolkitQueryKey = (projectId: string, toolkitId: string) =>
  ['deepwiki', 'toolkit', projectId, toolkitId] as const;

export function useWikiToolkit(
  projectId: string,
  toolkitId: string,
): UseQueryResult<WikiToolkitContext, Error> {
  return useQuery({
    queryKey: wikiToolkitQueryKey(projectId, toolkitId),
    // Both halves must be present. `enabled: false` keeps the query idle rather
    // than firing at `/elitea_core/tool/prompt_lib//` — a URL that 404s and
    // renders as a broken toolkit rather than as one not chosen yet.
    enabled: projectId !== '' && toolkitId !== '',
    queryFn: () => fetchWikiToolkit(projectId, toolkitId),
  });
}

/**
 * The `type` a wiki toolkit row carries.
 *
 * It is the provider's own toolkit name, lowercased — the descriptor names
 * this provider's single toolkit `Wikis` (conformance fixture
 * descriptor/legacy-v0/provider_descriptor.json) and the SPI addresses it as
 * `wikis` in `/tools/{toolkit_name}/{tool_name}/invoke`.
 *
 * NOTHING IN THIS SERVICE CREATES SUCH A ROW YET. `toolkitTypeSchemas` in
 * internal/api/v2/toolkits/handler.go is a hand-written map plus the pinned SDK
 * snapshot, and neither carries a provider-supplied type — that is the provider
 * hub, ADR-0012 phase P3. So today a wiki toolkit exists only where something
 * outside the product put it there (the legacy runtime, or this repository's
 * E2E seed), and this constant is what those two and this reader agree on.
 */
const WIKI_TOOLKIT_TYPE = 'wikis';

/**
 * A primitive rendered as text; anything else is NOT text.
 *
 * `String(someObject)` yields "[object Object]", which would make a malformed
 * row match nothing and render as a toolkit named after the failure.
 */
function scalar(value: unknown): string {
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean' || typeof value === 'bigint') {
    return String(value);
  }
  return '';
}

/** One row of the project's toolkit listing. */
export interface WikiToolkitSummary {
  readonly id: string;
  readonly name: string;
}

interface ToolkitListRow {
  readonly id?: unknown;
  readonly name?: unknown;
  readonly type?: unknown;
}

/**
 * The project's wiki toolkits.
 *
 * A project can hold several, each pointed at a different repository, which is
 * why the browser is addressed by toolkit. This is what makes that address
 * reachable without someone handing over a URL.
 */
export async function listWikiToolkits(projectId: string): Promise<WikiToolkitSummary[]> {
  const body = unwrapBody(
    await eliteaFetch<unknown>(`/elitea_core/tools/prompt_lib/${projectId}?limit=100`),
  ) as { rows?: readonly ToolkitListRow[] } | undefined;

  return (body?.rows ?? [])
    .filter((row) => scalar(row.type).toLowerCase() === WIKI_TOOLKIT_TYPE)
    .map((row) => ({
      id: scalar(row.id),
      // A row with no name is still selectable: the id is its address, and
      // hiding it would make a wiki unreachable over a missing label.
      name: typeof row.name === 'string' && row.name !== '' ? row.name : scalar(row.id),
    }))
    .filter((row) => row.id !== '');
}

const wikiToolkitListQueryKey = (projectId: string) =>
  ['deepwiki', 'toolkit', 'list', projectId] as const;

export function useWikiToolkits(projectId: string): UseQueryResult<WikiToolkitSummary[], Error> {
  return useQuery({
    queryKey: wikiToolkitListQueryKey(projectId),
    enabled: projectId !== '',
    queryFn: () => listWikiToolkits(projectId),
  });
}
