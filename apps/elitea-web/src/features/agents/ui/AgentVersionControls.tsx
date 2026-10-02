import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import type { ApplicationVersionDetail, VersionWriteRequest } from '@/shared/api/generated/model';

import { useVersionBarCommands } from '../model/useVersionBarCommands';
import type { AgentPipelineVersionOption } from '../lib/types';

import { AgentPipelineVersionSelector } from './AgentPipelineVersionSelector';
import { CompareVersionsButton } from './compare-versions/CompareVersionsButton';
import { DeleteVersionDialog } from './DeleteVersionDialog';
import { SaveNewVersionButton } from './SaveNewVersionButton';
import { SetDefaultVersionDialog } from './SetDefaultVersionDialog';

/**
 * The agent editor's version bar: the version dropdown plus "Save As
 * Version" — the pair the baseline mounts side by side in
 * `apps/elitea-ui/src/[fsd]/entities/application-tab-bar/ui/
 * ApplicationTabBar.jsx:58-68` (`<ApplicationVersionSelect/>` in the centred
 * block, `<SaveNewVersionButton/>` in the right-hand one, the latter gated on
 * `viewMode !== ViewMode.Public`).
 *
 * WHY THIS FILE EXISTS (#134). Both halves were already ported and both were
 * unreachable from the agent edit page: `AgentPipelineVersionSelector`'s only
 * production importer was `ToolCardBody.tsx` (a TOOL card, not the agent's own
 * page) and `SaveNewVersionButton` had no production importer at all. The page
 * even fetched the version list and spent it solely on a 404 check. This
 * component is the composition root the two were missing — one symbol on
 * `features/agents`' curated public API (§3.3 ≤20) instead of two.
 *
 * Deliberately dumb: every mutation-adjacent decision (which route a version
 * switch navigates to, what the cloned version body contains, cache
 * invalidation after a new version is created) stays with the page, matching
 * `AgentPipelineVersionSelector`'s own "the version-SWITCH mutation is
 * entirely caller-owned" contract.
 *
 * **SET DEFAULT AND DELETE VERSION (#147) are the two mutations this
 * component owns, and the reason is that the page has nothing to do with the
 * first and only navigates after the second.** JRNY-015's
 * middle step ("create a new version -> SET DEFAULT -> delete old") had no
 * UI at all: the route, the handler, the repo write and the generated
 * `setApplicationDefaultVersion` all existed, and nothing in the app called
 * any of them. A page-owned `onDefaultVersionSet` callback would have been
 * one more prop that only ever forwarded to a query invalidation that
 * changes nothing — see the disclosed read gap below — so the whole
 * affordance is mounted here, gated on the same `canSaveNewVersion` flag the
 * other two write controls use.
 *
 * **The current default IS readable now — it is fetched, and only
 * remembered as an override.** `applications.meta.default_version_id` is
 * written by `SetDefaultVersion` (`repos/applications.go:650-682`) and, since
 * the read half landed, reported by `GET /application/...` in two places:
 * `meta.default_version_id` on the application and `is_default` on each
 * `versions[]` entry (`applications/handler.go`'s `Get`/`getVersions`). The
 * options this component is handed carry that flag through
 * (`AgentPipelineVersionOption.is_default`), so the default shows on FIRST
 * render and survives a reload.
 *
 * The remembered id is now an OVERRIDE for the window between a successful
 * PATCH and the next detail fetch, not the only source. It wins while set,
 * because in that window the server flag this component was rendered with is
 * known-stale by exactly the write it just made. The same holds for a version
 * this component has seen deleted: it leaves the menu at once, not at the
 * next fetch.
 *
 * The one thing still not readable is a default recorded for a version the
 * list does not contain; the list is the whole version set, so that is a
 * database inconsistency rather than a gap.
 *
 * BOTH of those overrides, and the pending state of the two dialogs, live in
 * `../model/useVersionBarCommands`. They were inline here until the delete
 * half landed and put this component at a cyclomatic complexity of 17 against
 * the §3.5 budget of 12.
 */
export interface AgentVersionControlsProps {
  readonly applicationId: string;
  readonly projectId: string | undefined;
  readonly versions: readonly AgentPipelineVersionOption[];
  readonly activeVersionId: number | undefined;
  readonly onSelectVersion: (version: AgentPipelineVersionOption) => void;
  /**
   * The current version's fields, cloned onto the new one. `name` is supplied
   * by the dialog inside `SaveNewVersionButton`, which is why it is excluded
   * here (see `useSaveNewVersion`'s doc comment: `name` is the one field the
   * Go handler genuinely requires on this operation).
   */
  readonly versionBody: Omit<VersionWriteRequest, 'name'>;
  /** `false` for a read-only (public-project) viewer, mirroring `ApplicationTabBar.jsx:65` — the selector stays, only the write affordance goes. */
  readonly canSaveNewVersion: boolean;
  /**
   * Disables "Save As Version" ALONE, leaving the selector, "Set as default"
   * and "Delete version" alive.
   *
   * Deliberately separate from `canSaveNewVersion`, which is the
   * writer-vs-public-viewer gate and also governs those other two controls.
   * The pipelines editor needs to withhold this one button while the live
   * flow graph is one the runtime would refuse — that write persists the
   * graph, so it must obey the same veto the Save button does — and it must
   * NOT thereby take away the user's ability to delete a version or pin a
   * default, neither of which touches the canvas.
   */
  readonly saveNewVersionDisabled?: boolean | undefined;
  readonly onNewVersionSaved: (created: ApplicationVersionDetail) => void;
  readonly onNewVersionError?: ((message: string) => void) | undefined;
  /**
   * #307/#147 — version delete. `useDeleteVersion` was exported from this
   * slice's public API for "a not-yet-built version-delete dialog" and then
   * had no caller anywhere; `VersionReplacementModal`, its in-use branch, had
   * none either. Both hang off `DeleteVersionDialog`, composed here so the
   * version bar stays the single mount point for everything version-scoped.
   * Optional: a caller that cannot supply `onVersionDeleted` (nowhere to
   * navigate afterwards) gets no delete affordance rather than a dead one.
   *
   * #147 moved the TRIGGER into the version menu, beside "Set as default",
   * where the baseline puts it — see `DeleteVersionDialog`'s doc comment.
   * The version to delete now comes FROM THAT MENU (whichever version it
   * marks as selected), so the caller no longer names one. The old
   * `applicationVersionId`/`versionName` pair said "the page's active
   * version" and is gone: it could only ever delete the version the user was
   * already on, while the menu's item, like the baseline's, reaches any of
   * them.
   *
   * `onVersionDeleteError` is a SECOND reporting channel, not the only one:
   * the refusal is shown inside the dialog whether or not a caller supplies
   * this. Leaving it unset used to mean the refusal reached nobody.
   */
  readonly versionDelete?:
    | {
        readonly onVersionDeleted: () => void;
        readonly onVersionDeleteError?: ((message: string) => void) | undefined;
      }
    | undefined;
}

const wrapperSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.75rem' };

export function AgentVersionControls({
  applicationId,
  projectId,
  versions,
  activeVersionId,
  onSelectVersion,
  versionBody,
  canSaveNewVersion,
  saveNewVersionDisabled = false,
  onNewVersionSaved,
  onNewVersionError,
  versionDelete,
}: AgentVersionControlsProps): ReactNode {
  /*
   * Both commands' state lives in one hook (#147). Inline, the two flows put
   * this component at a cyclomatic complexity of 17 against the §3.5 budget
   * of 12 — and every branch they added was state, not layout. The hook hands
   * back finished dialog props, so the JSX below spreads them and decides
   * nothing.
   */
  const commands = useVersionBarCommands({
    applicationId,
    projectId,
    versions,
    canWrite: canSaveNewVersion,
    versionDelete,
  });

  return (
    <Box sx={wrapperSx}>
      {/* #compare-versions — offered from the version bar, the same place the
          baseline offers it (`ApplicationControls.jsx:172`'s dropdown item,
          gated there on `versions.length >= 2`). Read-only, so unlike the
          baseline's item it is NOT additionally gated on the update
          permission. */}
      <CompareVersionsButton
        projectId={projectId}
        applicationId={Number(applicationId)}
        versions={commands.visibleVersions}
        activeVersionId={activeVersionId}
      />
      {/* `onSetDefaultVersion`/`onDeleteVersion` are `undefined` when the
          command is not on offer, and the menu renders no item for an absent
          callback — see `AgentPipelineVersionSelector`'s prop docs. */}
      <AgentPipelineVersionSelector
        applicationVersionId={activeVersionId}
        versions={commands.visibleVersions}
        onSelectVersion={onSelectVersion}
        defaultVersionId={commands.defaultVersionId}
        onSetDefaultVersion={commands.onSetDefaultVersion}
        onDeleteVersion={commands.onDeleteVersion}
      />
      <SetDefaultVersionDialog {...commands.setDefaultDialog} />
      {/* #147 — the delete confirmation. The version it acts on is the one
          the MENU offered, not the page's active version: the menu's item
          works on whichever version the menu marks as selected, exactly as
          "Set as default" does. */}
      <DeleteVersionDialog {...commands.deleteDialog} />
      {canSaveNewVersion && (
        <SaveNewVersionButton
          applicationId={applicationId}
          projectId={projectId}
          existingVersionNames={commands.visibleVersions.map((version) => version.name)}
          version={versionBody}
          {...(activeVersionId === undefined ? {} : { sourceVersionId: activeVersionId })}
          disabled={saveNewVersionDisabled}
          onSuccess={onNewVersionSaved}
          {...(onNewVersionError === undefined ? {} : { onError: onNewVersionError })}
        />
      )}
    </Box>
  );
}
