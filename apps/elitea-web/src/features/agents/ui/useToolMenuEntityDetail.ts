import { useMemo } from 'react';

import { useGetApplication } from '@/shared/api/generated/applications/applications';
import type { ApplicationDetail } from '@/shared/api/generated/model';

import type { AgentToolAssociation } from '../lib/types';

export interface ToolMenuEntityDetail {
  readonly numericApplicationId: number | undefined;
  readonly versionId: number | undefined;
  readonly isEntityUnsaved: boolean;
  readonly entityLabel: 'agent' | 'pipeline';
  readonly currentTools: readonly AgentToolAssociation[];
  readonly addedToolkitIds: ReadonlySet<string | number>;
}

/** What the caller already knows about the version it edits; see `ToolMenuProps`. */
export interface PinnedVersion {
  readonly versionId?: number | string | undefined;
  readonly currentTools?: readonly AgentToolAssociation[] | undefined;
}

function toApplicationTool(tool: unknown): AgentToolAssociation {
  return tool as AgentToolAssociation;
}

function computeAddedToolkitIds(currentTools: readonly AgentToolAssociation[]): ReadonlySet<string | number> {
  const ids = currentTools
    .filter((tool) => tool.type !== 'application')
    .map((tool) => tool.id)
    .filter((id): id is string | number => id !== undefined);
  return new Set(ids);
}

function resolveEntityLabel(agentType: string | undefined): 'agent' | 'pipeline' {
  return agentType === 'pipeline' ? 'pipeline' : 'agent';
}

/** The pinned version wins; the application detail carries only the DEFAULT version (#2130). */
function resolveVersionId(pinned: PinnedVersion, defaultVersionId: number | undefined): number | undefined {
  return pinned.versionId === undefined ? defaultVersionId : Number(pinned.versionId);
}

/**
 * The application and version a ToolMenu is attached to: its own detail read
 * for the label and, when the caller does not name one, the default version.
 */
export function useToolMenuEntityDetail(
  applicationId: number | string | undefined,
  projectId: string | undefined,
  pinned: PinnedVersion,
): ToolMenuEntityDetail {
  const numericApplicationId = applicationId === undefined ? undefined : Number(applicationId);
  const applicationQuery = useGetApplication(projectId ?? '', numericApplicationId ?? 0, {
    query: { enabled: projectId !== undefined && numericApplicationId !== undefined },
  });
  // Same error-envelope-unreachable cast as every other Wave-2 generated-client caller (eliteaFetch throws instead of resolving with the error variant).
  const versionDetails = (applicationQuery.data as { data: ApplicationDetail } | undefined)?.data.version_details;
  const versionId = resolveVersionId(pinned, versionDetails?.id === undefined ? undefined : Number(versionDetails.id));
  const detailTools = useMemo(() => (versionDetails?.tools ?? []).map(toApplicationTool), [versionDetails?.tools]);
  const currentTools = pinned.currentTools ?? detailTools;
  const addedToolkitIds = useMemo(() => computeAddedToolkitIds(currentTools), [currentTools]);

  return {
    numericApplicationId,
    versionId,
    isEntityUnsaved: numericApplicationId === undefined || versionId === undefined,
    entityLabel: resolveEntityLabel(versionDetails?.agent_type),
    currentTools,
    addedToolkitIds,
  };
}
