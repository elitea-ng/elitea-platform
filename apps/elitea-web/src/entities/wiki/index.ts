/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 *
 * It carries only what a consumer imports TODAY, and grows one phase at a time.
 * `gate-dead-code` runs knip at `--max-issues 0`, so an export published ahead
 * of its consumer fails the build — which is the rule working: a helper with no
 * caller is dead code whether or not a barrel names it.
 *
 * The identity helpers are the reason this is an entity and not a feature:
 * every wiki feature needs them, and `no-sideways-features` forbids one feature
 * importing another.
 */
export type { RepositoryIdentity, ToolkitSettings, WikiManifest } from './model/types';
export { filterManifestsByRepo } from './lib/repoMatch';
// Added in P5, when the settings feature became its first consumer.
export { getConfiguredRepoIdentity, getCodeToolkitReference } from './lib/toolkitSettings';
// Added with the artifact-folder source: the settings form needs to know WHY
// a folder was refused, the two panels only need the folder itself.
export { getArtifactSource, readArtifactSource } from './lib/artifactSource';
// Added with the real-engine run: the page view needs the same key the fetch
// resolves, for the edit that saves back over it.
export { wikiPageObjectKey } from './lib/pageKey';
export { chatPinsFor } from './lib/chatPins';
export {
  fetchWikiManifest,
  fetchWikiPage,
  listWikiObjects,
  manifestKeys,
  putWikiPage,
} from './api/wikiArtifactsApi';
export { useWikiToolkit, useWikiToolkits } from './api/wikiToolkitApi';
