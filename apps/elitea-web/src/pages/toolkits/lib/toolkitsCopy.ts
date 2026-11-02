/**
 * What the toolkits list page says, on its toolkits or its MCPs surface
 * (`../Toolkits.tsx`): one surface's words and test ids in one place.
 */
import { t } from '@/shared/i18n';

export interface ToolkitsCopy {
  readonly title: string;
  readonly emptyTitle: string;
  readonly emptyDescription: string;
  readonly searchPlaceholder: string;
  readonly searchTestId: string;
  readonly createLabel: string;
  readonly createTestId: string;
  readonly listTestId: string;
  readonly summary: (count: number) => string;
}

export function toolkitsCopy(isMCP: boolean): ToolkitsCopy {
  if (isMCP) {
    return {
      title: t('pages.toolkits.toolkits.titleMcp', 'MCPs'),
      emptyTitle: t('pages.toolkits.toolkits.emptyMcpTitle', 'No MCPs yet'),
      emptyDescription: t('pages.toolkits.toolkits.emptyMcpDescription', 'Create your first MCP to get started.'),
      searchPlaceholder: t('pages.toolkits.toolkits.searchMcp', 'Search MCPs'),
      searchTestId: 'mcp-search-input',
      createLabel: t('pages.toolkits.toolkits.createMcp', 'New MCP'),
      createTestId: 'mcps-create',
      listTestId: 'mcps-list-panel',
      summary: (count) => t('pages.toolkits.toolkits.countMcp', '{{count}} MCPs', { count }),
    };
  }
  return {
    title: t('pages.toolkits.toolkits.title', 'Toolkits'),
    emptyTitle: t('pages.toolkits.toolkits.emptyTitle', 'No toolkits yet'),
    emptyDescription: t('pages.toolkits.toolkits.emptyDescription', 'Create your first toolkit to get started.'),
    searchPlaceholder: t('pages.toolkits.toolkits.search', 'Search toolkits'),
    searchTestId: 'toolkit-search-input',
    createLabel: t('pages.toolkits.toolkits.create', 'New toolkit'),
    createTestId: 'toolkits-create',
    listTestId: 'toolkits-list-panel',
    summary: (count) => t('pages.toolkits.toolkits.count', '{{count}} toolkits', { count }),
  };
}
