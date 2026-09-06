import type { AgentPipelineVersionOption } from './types';

/**
 * The one name the server treats as the application's own live version.
 *
 * Duplicated here for the same reason `AgentPipelineVersionSelector.tsx`
 * duplicates it: `no-sideways-*` boundaries make one small constant cheaper
 * than a new import edge for one string.
 */
const LATEST_VERSION_NAME = 'base';

/**
 * Which versions the delete item refuses, ported from the baseline's
 * `disableDelete` (`apps/elitea-ui/src/[fsd]/entities/application-tab-bar/ui/
 * ApplicationControls.jsx:100-105`):
 *
 *  - the version that IS the application's default. `DeleteVersion`
 *    (`services/elitea-main/internal/api/v2/applications/handler.go:1213`)
 *    deletes the row and leaves `applications.meta.default_version_id`
 *    pointing at it, so the application would report a default that no
 *    version answers to.
 *  - the version named "base". It is the application's live version; the
 *    editor and every toolkit reference resolve to it when no default is
 *    recorded.
 *
 * The published/embedded case is NOT here on purpose. The server owns it
 * (`handler.go:1223-1235` answers 400 "Unpublish first. Cannot delete a
 * published version.") and it can change between the render of this menu and
 * the click. A client-side copy of that rule would either hide the item on
 * stale data or, worse, disagree with the server and give the user no
 * message at all. The refusal is shown in the confirm dialog instead.
 *
 * `undefined` version — the list has not resolved — reads as "cannot delete".
 */
export function isDeleteVersionDisabled(
  version: AgentPipelineVersionOption | undefined,
  defaultVersionId: number | undefined,
): boolean {
  if (version === undefined) return true;
  if (version.name === LATEST_VERSION_NAME) return true;
  if (defaultVersionId !== undefined && version.id === defaultVersionId) return true;
  return false;
}
