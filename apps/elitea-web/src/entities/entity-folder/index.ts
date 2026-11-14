/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 * The project folders of the Agents, Pipelines, Skills, Toolkits and MCPs
 * lists (issue 2048).
 */
export { entityIdsInFolder, folderOfEntity, neighboursForStep } from './model/kinds';
export type { EntityFolderKind } from './model/kinds';
export { useEntityFolderMutations, useEntityFolders } from './model/useEntityFolders';
export type { EntityFolder } from '@/shared/api/generated/model';
