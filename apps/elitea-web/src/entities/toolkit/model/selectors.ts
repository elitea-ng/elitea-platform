import type { Toolkit } from './types';

/**
 * Display-name fallback chain — apps/elitea-ui/src/[fsd]/features/toolkits/
 * ui/list/ToolkitsList.jsx:134-144 `getToolkitItemName`: `name` ->
 * `toolkit_name` -> `settings.elitea_title` -> `settings.configuration_title`
 * -> capitalized `type`. `toolkit_name` isn't part of `Toolkit`'s modeled
 * shape (backend instance evidence has no such field — see `./types.ts`'s
 * doc comment), so it's read defensively the same way
 * `ToolkitsOperationButtons.tsx:93` reads an unmodeled property off an
 * `unknown`-typed value, matching the baseline's real fallback chain
 * instead of silently dropping a branch.
 */
export function toolkitDisplayName(toolkit: Toolkit): string {
  if (toolkit.name.trim() !== '') return toolkit.name;
  const rawToolkitName = (toolkit as unknown as { readonly toolkit_name?: unknown }).toolkit_name;
  if (typeof rawToolkitName === 'string' && rawToolkitName.trim() !== '') return rawToolkitName;
  const eliteaTitle = toolkit.settings?.['elitea_title'];
  if (typeof eliteaTitle === 'string' && eliteaTitle.trim() !== '') return eliteaTitle;
  const configurationTitle = toolkit.settings?.['configuration_title'];
  if (typeof configurationTitle === 'string' && configurationTitle.trim() !== '') return configurationTitle;
  return toolkit.type.charAt(0).toUpperCase() + toolkit.type.slice(1);
}

/**
 * apps/elitea-ui/src/[fsd]/shared/lib/helpers/mcp.helpers.js:7-14
 * `isMcpToolkitType`/`isMcpToolkit` — `type === 'mcp'`, a `mcp_*` pre-built
 * type, or `meta.mcp === true`.
 */
export function isMcpToolkit(toolkit: Toolkit): boolean {
  if (toolkit.type === 'mcp' || toolkit.type.startsWith('mcp_')) return true;
  return toolkit.meta?.['mcp'] === true;
}

/**
 * apps/elitea-ui/src/[fsd]/pages/toolkits/components/Card.jsx:298-300 —
 * "is connected" is server-pushed `online` OR (for MCP toolkits) a live
 * client OAuth token; the token side is out of scope for a pure selector
 * (it reads localStorage) and is left to the `mcp` feature layer. This
 * selector covers the server-pushed half.
 */
export function isOnlineToolkit(toolkit: Toolkit): boolean {
  return toolkit.online === true;
}

/** Alphabetical name sort using the same display-name fallback as `toolkitDisplayName`. */
export function sortToolkitsByName(toolkits: readonly Toolkit[]): Toolkit[] {
  return [...toolkits].sort((a, b) =>
    toolkitDisplayName(a).toLowerCase().localeCompare(toolkitDisplayName(b).toLowerCase()),
  );
}
