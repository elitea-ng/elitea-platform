/**
 * What a requirement can be bound to in the project that hires: its toolkits
 * for a toolkit or MCP requirement, its agents for a nested-agent requirement
 * and its skills for a skill requirement. Read only while the wizard is open.
 */
import { useCallback, useMemo } from 'react';

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
  /** The project's options for one requirement. A nested agent has none: it comes with the agent. */
  readonly optionsFor: (requirement: MarketplaceRequirement) => readonly BindingOption[];
  /** The toolkit and skill lists are both still loading. */
  readonly isPending: boolean;
}

/** `application` rows are agents used as tools, not toolkits. */
const isToolkit = (row: NamedRow): boolean => row.type !== 'application';

export function useBindingTargets(projectId: string | undefined, open: boolean): BindingTargets {
  const enabled = open && projectId !== undefined;
  const toolkits = useListToolkits(projectId ?? '', { query: { enabled } });
  const skills = useSkills(enabled ? projectId : undefined, '');
  const toolkitRows = unwrapList<NamedRow>(toolkits.data?.data, 'listToolkits(marketplace-hire)');
  const skillRows = useMemo<readonly NamedRow[]>(() => skills.data?.items ?? [], [skills.data]);

  const optionsFor = useCallback(
    (requirement: MarketplaceRequirement): readonly BindingOption[] => {
      // Agents are informational: they are copied with the listing, so there is nothing to bind.
      if (requirement.kind === 'agent') return [];
      if (requirement.kind === 'skill') return skillRows.map(optionOf);
      // A toolkit of the required type; an MCP requirement also takes the generic `mcp` toolkits.
      return toolkitRows
        .filter(isToolkit)
        .filter((row) => row.type === requirement.type || (requirement.kind === 'mcp' && row.type === 'mcp'))
        .map(optionOf);
    },
    [toolkitRows, skillRows],
  );
  return { optionsFor, isPending: toolkits.isPending || skills.isPending };
}
