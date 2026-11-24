/**
 * Options for the DB action rule's SQL credential picker: the project's `sql`
 * configurations, read through the generated `listConfigurations` client.
 * Every DB action rule requires a `sql` configuration reference (host and
 * credentials never live in pipeline YAML or rule rows). A stored id missing
 * from the page keeps its own option so editing never silently clears it.
 */
import { useMemo } from 'react';

import { useListConfigurations } from '@/shared/api/generated/configurations/configurations';
import type { ConfigurationListResponse } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import type { SingleSelectOption } from '@/shared/ui/SingleSelect';

const SQL_PARAMS = { type: ['sql'], limit: 200 };

export function useSqlCredentialOptions(projectId: string, enabled: boolean, currentId: number): SingleSelectOption[] {
  const query = useListConfigurations(projectId, SQL_PARAMS, { query: { enabled: !!projectId && enabled } });
  const body = query.data?.data as ConfigurationListResponse | undefined;

  return useMemo(() => {
    const options: SingleSelectOption[] = [
      { value: '0', label: t('entities.dbActionRule.form.credentialSelect', 'Select a SQL configuration') },
      ...(body?.items ?? [])
        .filter((row) => row.id !== undefined)
        .map((row) => ({ value: String(row.id), label: `${row.label || row.elitea_title || row.type} (${row.id})` })),
    ];
    if (currentId > 0 && !options.some((option) => option.value === String(currentId))) {
      options.push({ value: String(currentId), label: String(currentId) });
    }
    return options;
  }, [body, currentId]);
}
