/**
 * Ported from the baseline's `consts.js` (Wave-2 promotion pass, Part 2).
 * The promotion brief pointed at
 * `apps/elitea-ui/src/[fsd]/features/toolkits/lib/constants/
 * toolkitForm.constants.js` (47 lines, only `ToolEvents`/`ToolTypes`) — read
 * in full, but that is NOT the ~487-line file with `ToolInitialValues` the
 * brief described. The real file matching that description is
 * `apps/elitea-ui/src/pages/Applications/Components/Tools/consts.js`
 * (487 lines, confirmed via `wc -l`) — this port is from THAT file, plus
 * its sibling `toolOptions.js` (233 lines) which it imports from. Verified
 * by reading both in full, not by trusting the brief's pointer.
 *
 * Split across three files (this one, `toolOptions.ts`, and
 * `toolInitialValues.ts`) purely to stay under the §3.5 400-line-per-file
 * budget — `consts.js` alone is 487 lines. `ToolTypes`/`ToolEvents` live
 * here; the 25 per-type option arrays are in `toolOptions.ts`; the
 * `ToolInitialValues` map built from both is in `toolInitialValues.ts`.
 */

/** Event names dispatched around the tool-configuration flow (`ToolEvents` in the baseline). */
export const ToolEvents = {
  ValidateToolEvent: 'ValidateToolEvent',
  SaveEvent: 'SaveEvent',
  ResetValidateEvent: 'ResetValidateEvent',
  ToolkitsCreateBackEvent: 'ToolkitsCreateBackEvent',
  ToolkitsCreateToolkit: 'ToolkitsCreateToolkit',
  ToolkitsUpdateToolkit: 'ToolkitsUpdateToolkit',
  ToolkitsCreateToolkitWithConfiguration: 'ToolkitsCreateToolkitWithConfiguration',
} as const;

/** Why a `ValidateToolEvent` fired (`ValidateToolEventReason` in the baseline). */
export const ValidateToolEventReason = {
  createAgent: 'createAgent',
  saveNewVersion: 'saveNewVersion',
  saveLatestVersion: 'saveLatestVersion',
} as const;

/**
 * The FE-owned tool-type label/value catalogue (`ToolTypes` in the
 * baseline) — this is what `entities/toolkit`'s `toolkitTypeMenuEntries`
 * (`./toolMenu.ts`, ported from the baseline's `useToolMenuItems.jsx`) uses
 * to override backend `metadata.label` values, and what `ToolInitialValues`
 * (`./toolInitialValues.ts`) is keyed by.
 */
export const ToolTypes = {
  ado_boards: { label: 'ADO Boards', value: 'ado_boards' },
  ado_plans: { label: 'ADO Plans', value: 'ado_plans' },
  ado_repos: { label: 'ADO Repos', value: 'ado_repos' },
  ado_wiki: { label: 'ADO Wiki', value: 'ado_wiki' },
  application: { label: 'Agent', value: 'application' },
  artifact: { label: 'Artifact', value: 'artifact' },
  bitbucket: { label: 'Bitbucket', value: 'bitbucket' },
  confluence: { label: 'Confluence', value: 'confluence' },
  custom: { label: 'Custom', value: 'custom' },
  github: { label: 'GitHub', value: 'github' },
  gitlab: { label: 'GitLab', value: 'gitlab' },
  gitlab_org: { label: 'GitLab Org', value: 'gitlab_org' },
  google: { label: 'Google', value: 'google' },
  google_places: { label: 'Google Places', value: 'google_places' },
  image_generation_model: { label: 'Image Generation', value: 'image_generation_model' },
  jira: { label: 'Jira', value: 'jira' },
  openapi: { label: 'OpenAPI', value: 'openapi' },
  open_api: { label: 'OpenAPI', value: 'openapi' },
  qtest: { label: 'QTest', value: 'qtest' },
  rally: { label: 'Rally', value: 'rally' },
  report_portal: { label: 'Report Portal', value: 'report_portal' },
  service_now: { label: 'ServiceNow', value: 'service_now' },
  sharepoint: { label: 'SharePoint', value: 'sharepoint' },
  sonar: { label: 'Sonar', value: 'sonar' },
  sql: { label: 'SQL', value: 'sql' },
  testio: { label: 'TestIO', value: 'testio' },
  testrail: { label: 'TestRail', value: 'testrail' },
  xray_cloud: { label: 'XRAY Cloud', value: 'xray_cloud' },
  yagmail: { label: 'Yagmail', value: 'yagmail' },
  zephyr: { label: 'Zephyr', value: 'zephyr' },
  zephyr_enterprise: { label: 'Zephyr Enterprise', value: 'zephyr_enterprise' },
  zephyr_essential: { label: 'Zephyr Essential', value: 'zephyr_essential' },
  zephyr_scale: { label: 'Zephyr Scale', value: 'zephyr_scale' },
  zephyr_squad: { label: 'Zephyr Squad', value: 'zephyr_squad' },
} as const;

/** `hostingOptions` in the baseline — cloud-vs-server toggle used by a handful of toolkit-settings forms. */
export const hostingOptions = [
  { label: 'Cloud', value: true },
  { label: 'Server', value: false },
] as const;

/**
 * `toolIconStaticURL` in the baseline — a module-level constant derived
 * from `VITE_SERVER_URL`. This app has no such module-scope env read
 * (`shared/config`'s `getConfig()` is a runtime call, not an import-time
 * constant — R-S2 forbids store/config reads at module scope), so this is
 * a pure function instead: pass in the resolved `vite_server_url` config
 * value (`shared/config`'s `Config['vite_server_url']`) and get the same
 * URL back.
 */
export function toolIconStaticUrl(serverUrl: string): string {
  const trimmed = serverUrl.endsWith('/') ? serverUrl.replace('/api/v2/', '') : serverUrl.replace('/api/v2', '');
  return `${trimmed}/app/application_tool_icon`;
}
