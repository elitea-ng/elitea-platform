import type { ReactNode } from 'react';
import { useCallback, useMemo, useRef, useState } from 'react';

import type { SxProps, Theme } from '@mui/material/styles';

import type { VersionToolRef } from '@/shared/api/generated/model';

import { useSelectedProjectId } from '../api/useSelectedProjectId';
import type { ToolRemovalUpdate } from '../lib/hooks/useDisassociateToolkit.hooks';
import type { AgentToolAssociation } from '../lib/types';
import { toAgentToolAssociations } from '../lib/versionTools';

import { AgentToolRow } from './AgentToolRow';
import { ApplicationTools } from './ApplicationTools';

/**
 * The composition root for an agent's Tools panel — the piece #307 named as
 * missing. `ApplicationTools` (accordion + `ToolMenu` + internal-tool
 * switches), `ToolCard` and `useDisassociateToolkit` were all ported,
 * tested, and imported by NOTHING; each takes a contract only a real
 * composition can satisfy (`ApplicationTools`' required `renderToolCard`
 * render-prop, `ToolCard`'s five prop GROUPS, `useDisassociateToolkit`'s
 * per-row `index` — one hook call per row, so a per-row COMPONENT is
 * unavoidable: `./AgentToolRow.tsx`).
 *
 * Lives in `features/agents` rather than in the page because everything it
 * assembles is intra-slice: composing it here spends ONE slot of this
 * slice's ≤20-symbol public API (§3.5) instead of the four
 * (`ApplicationTools`, `ToolCard`, `useDisassociateToolkit`,
 * `AgentToolAssociation`) a page-level composition would have needed
 * against a barrel with one slot free. The page keeps only the wiring that
 * is genuinely page state — see `pages/agents/ui/EditApplicationToolsPanel.tsx`.
 *
 * **Both write paths reach the database — verified against the Go source,
 * not inferred from endpoint names.** Attaching (`ToolMenu` -> `PATCH
 * /elitea_core/tool/prompt_lib/{project}/{toolkit}`, `has_relation: true`)
 * inserts an `entity_tool_mapping` row; detaching (the same route with
 * `has_relation: false`, or `updateApplicationRelation` for sub-agent rows)
 * deletes it; and `applications/handler.go`'s `fetchVersionDetails` reads
 * that same table straight back into `version_details.tools[]`. Neither
 * goes through the page's Save button: both are immediate server writes.
 * `tools` is therefore a MIRROR of server state here, not form state —
 * hence `useToolsMirror` below rather than a controlled prop.
 *
 * **`isVersionLocked` (gate restored).** The baseline
 * (`ApplicationTools.jsx:33-34,142`) reads `version_details.status` and
 * passes `disabled || isVersionLocked` to every `ToolCard`, locking tool
 * removal on `published`/`embedded` versions; the port dropped it along
 * with the ambient Formik context it read the status from. The Go relation
 * endpoint enforces the same rule server-side ("Cannot change tools on a
 * published version. Unpublish first."), so without it the UI offered a
 * Remove button whose only possible outcome was a 400. Applied to the CARDS
 * only, exactly as the baseline does — the attach menu's visibility keys on
 * `readOnly` alone.
 */
/** Structural only — built inline by the caller against `AgentToolsPanelProps`; un-exported so knip does not flag an unused named export. */
interface AgentToolsPanelEntity {
  readonly applicationId: number | undefined;
  readonly versionId: number | undefined;
  /** `version_details.status` — `published`/`embedded` lock tool changes (see the module doc comment). */
  readonly versionStatus?: string | undefined;
  /** `version_details.meta.attachment_toolkit_id`. */
  readonly attachmentToolkitId?: number | string | undefined;
}

/** Structural only, see `AgentToolsPanelEntity` above. */
interface AgentToolsPanelInternalTools {
  readonly value: readonly string[];
  readonly onChange: (next: readonly string[]) => void;
}

export interface AgentToolsPanelProps {
  readonly entity: AgentToolsPanelEntity;
  /** The server's own `version_details.tools[]`. Re-seeds the local mirror whenever a fetch returns a new array. */
  readonly versionTools: readonly VersionToolRef[] | undefined;
  /** Whether the host form already had unsaved edits — gates `useDisassociateToolkit`'s cache-splice-on-unmount signal. */
  readonly dirty: boolean;
  readonly internalTools: AgentToolsPanelInternalTools;
  /** Fired after a successful ATTACH so the caller can refetch the application detail this panel's `versionTools` comes from. */
  readonly onToolsChanged?: (() => void) | undefined;
  /** Read-only viewer (public project): hides the attach menu and disables per-card actions, matching the baseline's `disabled`. */
  readonly readOnly?: boolean | undefined;
  /**
   * Forwarded to `ApplicationTools`, which narrows the MODULES grid to
   * `attachments` alone and drops the "Show all" toggle for a pipeline
   * (baseline `ApplicationTools.jsx:91-94`). That branch already existed and
   * had no way in: this panel is the only mount point for `ApplicationTools`
   * in the app, and it hardcoded the agent default, so the pipeline editor
   * would have offered switches for agent-executor features its runtime never
   * reads. Defaults to `false`, so the agent editor is unchanged.
   */
  readonly isPipeline?: boolean | undefined;
  /** `entities/application-form`'s `viewMode` string, threaded into `ToolCard`'s "open in new tab" URL. */
  readonly viewMode: string;
  readonly sx?: SxProps<Theme> | undefined;
}

function isVersionLocked(status: string | undefined): boolean {
  return status === 'published' || status === 'embedded';
}

interface ToolsMirror {
  readonly tools: readonly AgentToolAssociation[];
  readonly initialTools: readonly AgentToolAssociation[];
  readonly onToolsChange: (tools: readonly AgentToolAssociation[]) => void;
  readonly onToolRemoved: (update: ToolRemovalUpdate) => void;
}

/**
 * Local mirror of the server's `tools[]`. Re-seeded (render-phase, not in
 * an effect — an effect renders one frame of the OLD list over data that
 * has already arrived) whenever the fetched array identity changes, which
 * is exactly when a refetch has returned: after an attach
 * (`onToolsChanged` -> the caller invalidates) or a version switch.
 *
 * A removal updates the mirror LOCALLY and does not ask for a refetch:
 * `useDisassociateToolkit` already calls `setRefetch()` for the
 * splice-on-unmount path whose whole purpose is to avoid a real refetch
 * fighting the form's other unsaved edits (see that hook's deviation 5).
 */
function useToolsMirror(versionTools: readonly VersionToolRef[] | undefined): ToolsMirror {
  const seed = useMemo(() => toAgentToolAssociations(versionTools), [versionTools]);
  const [tools, setTools] = useState<readonly AgentToolAssociation[]>(seed);
  const [initialTools, setInitialTools] = useState<readonly AgentToolAssociation[]>(seed);
  const seededFrom = useRef(versionTools);

  if (seededFrom.current !== versionTools) {
    seededFrom.current = versionTools;
    setTools(seed);
    setInitialTools(seed);
  }

  const onToolRemoved = useCallback((update: ToolRemovalUpdate) => {
    setTools(update.tools);
    setInitialTools(update.initialTools);
  }, []);

  return { tools, initialTools, onToolsChange: setTools, onToolRemoved };
}

export function AgentToolsPanel({ entity, versionTools, dirty, internalTools, onToolsChanged, readOnly = false, isPipeline = false, viewMode, sx }: AgentToolsPanelProps): ReactNode {
  const projectId = useSelectedProjectId();
  const mirror = useToolsMirror(versionTools);
  const rowDisabled = readOnly || isVersionLocked(entity.versionStatus);

  const rowEntity = useMemo(
    () => ({
      applicationId: entity.applicationId,
      versionId: entity.versionId,
      projectId,
      attachmentToolkitId: entity.attachmentToolkitId,
    }),
    [entity.applicationId, entity.versionId, entity.attachmentToolkitId, projectId],
  );

  const rowToolsState = useMemo(
    () => ({ tools: mirror.tools, initialTools: mirror.initialTools, dirty, onToolsChange: mirror.onToolsChange, onToolRemoved: mirror.onToolRemoved }),
    [mirror.tools, mirror.initialTools, mirror.onToolsChange, mirror.onToolRemoved, dirty],
  );

  const renderToolCard = useCallback(
    (tool: AgentToolAssociation, index: number, isDuplicate: boolean) => (
      <AgentToolRow
        tool={tool}
        index={index}
        isDuplicate={isDuplicate}
        disabled={rowDisabled}
        viewMode={viewMode}
        entity={rowEntity}
        toolsState={rowToolsState}
      />
    ),
    [rowDisabled, viewMode, rowEntity, rowToolsState],
  );

  return (
    <ApplicationTools
      tools={mirror.tools}
      internalTools={internalTools.value}
      onInternalToolsChange={internalTools.onChange}
      applicationId={entity.applicationId}
      renderToolCard={renderToolCard}
      onToolsChanged={onToolsChanged}
      disabled={readOnly}
      isPipeline={isPipeline}
      sx={sx}
    />
  );
}
