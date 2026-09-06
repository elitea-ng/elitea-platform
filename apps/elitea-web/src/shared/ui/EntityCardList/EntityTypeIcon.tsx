import type { ReactNode } from 'react';

import { ApplicationsIcon } from '@/shared/ui/icons/applications-icon';
import { FileIcon } from '@/shared/ui/icons/file-icon';
import { FlowIcon } from '@/shared/ui/icons/flow-icon';
import { KeyIcon } from '@/shared/ui/icons/key-icon';
import { McpIcon } from '@/shared/ui/icons/mcp-icon';
import { SkillIcon } from '@/shared/ui/icons/skill-icon';
import { ToolsIcon } from '@/shared/ui/icons/tools-icon';

/**
 * The glyph inside a card's (or table row's) round gradient tile, keyed by
 * entity type — ported from `apps/elitea-ui/src/components/EntityIcon.jsx`'s
 * `EntityTypeIcon` switch. `credential` and `toolkit` are additions: the
 * baseline falls through to its emoji placeholder for both, while the
 * reference screenshots show a real per-domain glyph.
 */
export type EntityKind = 'agent' | 'pipeline' | 'skill' | 'toolkit' | 'mcp' | 'credential' | 'index' | 'application';

const ICONS: Record<EntityKind, () => ReactNode> = {
  agent: () => <ApplicationsIcon />,
  application: () => <ApplicationsIcon />,
  pipeline: () => <FlowIcon />,
  skill: () => <SkillIcon />,
  toolkit: () => <ToolsIcon />,
  mcp: () => <McpIcon />,
  credential: () => <KeyIcon />,
  index: () => <FileIcon />,
};

export function entityTypeIcon(kind: EntityKind): ReactNode {
  return ICONS[kind]();
}
