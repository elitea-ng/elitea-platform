/**
 * What a requirement can be bound to in the project that hires: its toolkits
 * for a toolkit or MCP requirement, its agents for a nested-agent requirement
 * and its skills for a skill requirement. Read only while the wizard is open.
 */
import { useCallback, useMemo } from 'react';

import { useListApplications } from '@/shared/api/generated/applications/applications';
import type { MarketplaceRequirement } from '@/shared/api/generated/model';
import { useListToolkits } from '@/shared/api/generated/toolkits/toolkits';
import { unwrapList } from '@/shared/api/unwrap';
import { useSkills } from '@/features/skills';

export interface BindingOption {
  readonly value: string;
  readonly label: string;
}

interface NamedRow {
  readonly id?: string | number;
  readonly name?: string;
  readonly type?: string;
}

const optionOf = (row: NamedRow): BindingOption => ({ value: String(row.id ?? ''), label: row.name ?? '' });

export interface BindingTargets {
  /** The project's options for one requirement. */
  readonly optionsFor: (requirement: MarketplaceRequirement) => readonly BindingOption[];
  readonly isPending: boolean;
}

export function useBindingTargets(projectId: string | undefined, open: boolean): BindingTargets {
  const enabled = open && projectId !== undefined;
  const toolkits = useListToolkits(projectId ?? '', { query: { enabled } });
  const agents = useListApplications(projectId ?? '', { agents_type: 'classic' }, { query: { enabled } });
  const skills = useSkills(enabled ? projectId : undefined, '');
  const toolkitRows = unwrapList<NamedRow>(toolkits.data?.data, 'listToolkits(marketplace-hire)');
  const agentRows = unwrapList<NamedRow>(agents.data?.data, 'listApplications(marketplace-hire)');
  const skillRows = useMemo<readonly NamedRow[]>(() => skills.data?.items ?? [], [skills.data]);

  const optionsFor = useCallback(
    (requirement: MarketplaceRequirement): readonly BindingOption[] => {
      if (requirement.kind === 'agent') return agentRows.map(optionOf);
      if (requirement.kind === 'skill') return skillRows.map(optionOf);
      // A toolkit of the required type; an MCP requirement also takes the generic `mcp` toolkits.
      return toolkitRows
        .filter((row) => row.type === requirement.type || (requirement.kind === 'mcp' && row.type === 'mcp'))
        .map(optionOf);
    },
    [toolkitRows, agentRows, skillRows],
  );
  return { optionsFor, isPending: toolkits.isPending || agents.isPending };
}
