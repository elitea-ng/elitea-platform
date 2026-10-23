/**
 * The "/" picker of a model chat, in two phases over one token:
 *
 *   "/gi"            phase 1: the conversation's toolkits, filtered by "gi"
 *   "/github/"       phase 2: that toolkit's tools
 *   "/github/list"   phase 2, filtered by "list"
 *
 * A toolkit pick inserts "/<Toolkit>/" (the token stays open for phase 2);
 * a tool pick inserts "/<Toolkit>/<tool> ". Text only: nothing changes in
 * what is sent besides the words.
 */

export interface ToolkitOption {
  /** The toolkit's id (`entity_meta.id`), for listToolkitAvailableTools. */
  toolkitId: number;
  name: string;
}

export type SlashPhase =
  | { phase: 'toolkit'; filter: string }
  | { phase: 'tool'; toolkit: ToolkitOption; filter: string }
  /** The text before the second slash names no toolkit of this conversation. */
  | { phase: 'unknown_toolkit'; name: string };

export function slashPhase(query: string, toolkits: readonly ToolkitOption[]): SlashPhase {
  const slash = query.indexOf('/');
  if (slash < 0) return { phase: 'toolkit', filter: query };
  const name = query.slice(0, slash);
  const toolkit = toolkits.find((t) => t.name.toLocaleLowerCase() === name.toLocaleLowerCase());
  if (!toolkit) return { phase: 'unknown_toolkit', name };
  return { phase: 'tool', toolkit, filter: query.slice(slash + 1) };
}

/** A token-safe spelling of a name (no whitespace or slashes inside the token). */
export function tokenName(name: string): string {
  return name.trim().replace(/[\s/]+/g, '_');
}

export function matches(name: string, filter: string): boolean {
  return name.toLocaleLowerCase().includes(filter.toLocaleLowerCase());
}

export function toolkitInsert(toolkit: ToolkitOption): string {
  return `/${tokenName(toolkit.name)}/`;
}

export function toolInsert(toolkit: ToolkitOption, tool: string): string {
  return `/${tokenName(toolkit.name)}/${tokenName(tool)} `;
}

export function skillInsert(skill: string): string {
  return `~${tokenName(skill)} `;
}
