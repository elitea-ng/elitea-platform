import type { ReactNode } from 'react';

import { ToolBase, type ToolBaseProps } from './ToolBase';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/toolkits/ui/form/
 * ToolBase/ToolConfluence.jsx` (34 lines) — `ToolBase` pre-configured with
 * Confluence's field ordering/advanced-fields/excluded-fields lists.
 *
 * R1 fix: `ToolBaseProps` is flat now (see `ToolBase.types.ts`'s own module
 * doc comment) — `excludedFields`/`advancedFields`/`priorityFieldsOrder` are
 * overridden as top-level props here, matching the baseline's own flat
 * override, not nested inside a `fieldOrder` group.
 */
const CONFLUENCE_EXCLUDED_FIELDS = ['cloud'] as const;
const CONFLUENCE_ADVANCED_FIELDS = ['max_pages', 'number_of_retries', 'min_retry_seconds', 'max_retry_seconds', 'custom_headers'] as const;
const CONFLUENCE_PRIORITY_FIELDS = ['confluence_configuration', 'embedding_model', 'api_version', 'space', 'limit', 'labels'] as const;

export function ToolConfluence(props: ToolBaseProps): ReactNode {
  const existingExcluded = props.excludedFields ?? [];
  return (
    <ToolBase
      {...props}
      excludedFields={[...existingExcluded, ...CONFLUENCE_EXCLUDED_FIELDS]}
      advancedFields={CONFLUENCE_ADVANCED_FIELDS}
      priorityFieldsOrder={CONFLUENCE_PRIORITY_FIELDS}
    />
  );
}
