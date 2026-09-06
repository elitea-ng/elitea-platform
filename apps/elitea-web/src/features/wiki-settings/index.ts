/** Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20). */
export type { ParsedSettings, SettingsHint, SettingsProblem } from './lib/settingsForm';
export { canSaveSettings, parseSettingsDraft, withArtifactSource } from './lib/settingsForm';
export type { DeleteWikiResult } from './api/wikiSettingsApi';
export { useDeleteWiki, useSaveWikiSettings } from './api/wikiSettingsApi';
