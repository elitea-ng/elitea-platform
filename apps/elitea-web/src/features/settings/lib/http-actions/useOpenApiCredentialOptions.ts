/**
 * Options for the HTTP action rule's credential picker: "None" (0) plus the
 * project's `openapi` configurations, read through the generated
 * `listConfigurations` client. A rule whose stored id is not in the page
 * (deleted, or past the 200-row page) keeps an option of its own so the
 * select never silently shows "None" for a rule that has a credential.
 */
import { useMemo } from 'react';

import { useListConfigurations } from '@/shared/api/generated/configurations/configurations';
import type { ConfigurationListResponse } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import type { SingleSelectOption } from '@/shared/ui/SingleSelect';

const OPENAPI_PARAMS = { type: ['openapi'], limit: 200 };

export function useOpenApiCredentialOptions(projectId: string, enabled: boolean, currentId: number): SingleSelectOption[] {
  const query = useListConfigurations(projectId, OPENAPI_PARAMS, { query: { enabled: !!projectId && enabled } });
  const body = query.data?.data as ConfigurationListResponse | undefined;

  return useMemo(() => {
    const options: SingleSelectOption[] = [
      { value: '0', label: t('entities.httpActionRule.form.credentialNone', 'None') },
      ...(body?.items ?? [])
        .filter((row) => row.id !== undefined)
        .map((row) => ({ value: String(row.id), label: `${row.label || row.elitea_title || row.type} (${row.id})` })),
    ];
    if (currentId !== 0 && !options.some((option) => option.value === String(currentId))) {
      options.push({ value: String(currentId), label: String(currentId) });
    }
    return options;
  }, [body, currentId]);
}
