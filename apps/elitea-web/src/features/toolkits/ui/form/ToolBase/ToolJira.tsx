import type { ReactNode } from 'react';

import { ToolBase, type ToolBaseProps } from './ToolBase';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/toolkits/ui/form/
 * ToolBase/ToolJira.jsx` (27 lines) — `ToolBase` pre-configured with Jira's
 * field ordering/advanced-fields/excluded-fields lists.
 *
 * R1 fix: `ToolBaseProps` is flat now (see `ToolBase.types.ts`'s own module
 * doc comment) — `excludedFields`/`advancedFields`/`priorityFieldsOrder` are
 * overridden as top-level props here, matching the baseline's own flat
 * override (`ToolJira.jsx`: `<ToolBase {...props} excludedFields={...}
 * advancedFields={...} priorityFieldsOrder={...} />`), not nested inside a
 * `fieldOrder` group.
 */
const JIRA_EXCLUDED_FIELDS = ['cloud'] as const;
const JIRA_ADVANCED_FIELDS = ['verify_ssl', 'additional_fields', 'custom_headers'] as const;
const JIRA_PRIORITY_FIELDS = ['jira_configuration', 'embedding_model', 'api_version', 'limit', 'labels'] as const;

export function ToolJira(props: ToolBaseProps): ReactNode {
  const existingExcluded = props.excludedFields ?? [];
  return (
    <ToolBase
      {...props}
      excludedFields={[...existingExcluded, ...JIRA_EXCLUDED_FIELDS]}
      advancedFields={JIRA_ADVANCED_FIELDS}
      priorityFieldsOrder={JIRA_PRIORITY_FIELDS}
    />
  );
}
