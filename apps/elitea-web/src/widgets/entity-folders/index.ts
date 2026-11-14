/**
 * Public API — the project folders of the Agents, Pipelines, Skills,
 * Toolkits and MCPs lists (issue 2048). One scope and one panel serve all
 * five pages: the scope puts "Move to folder" on every card and a Folder
 * column on the table, the panel is the Folders section of the Filters tab.
 */
export { EntityFolderPanel } from './ui/EntityFolderPanel';
/** @public */
export type { EntityFolderPanelProps } from './ui/EntityFolderPanel';
export { EntityFolderScope } from './ui/EntityFolderScope';
/** @public */
export type { EntityFolderScopeProps } from './ui/EntityFolderScope';
export { folderIdParam, useFolderFilter } from './lib/useFolderFilter';
export type { FolderFilter } from './lib/useFolderFilter';
