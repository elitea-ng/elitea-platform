import { useCallback, useMemo } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { useAutomaticActions, useWorkforcePolicyActions } from '@/features/agent-workforce';
import { getListApplicationsQueryKey } from '@/shared/api/generated/applications/applications';
import type { Application } from '@/shared/api/generated/model';

/**
 * The 14-day Undo of an automatic hire, offered on its Workshop list row
 * (ADR-0051 decision 6, issue 2091). The action list is read only while a row
 * on the page is on probation, and a person without the workforce permission
 * gets an error from it, which reads as "no Undo to offer".
 */
export function useProbationUndo(projectId: string, rows: readonly Application[]): (application: Application) => (() => void) | undefined {
  const client = useQueryClient();
  const onProbation = rows.some((row) => row.meta?.['on_probation'] === true);
  const actions = useAutomaticActions(onProbation ? projectId : '');
  const { decide } = useWorkforcePolicyActions(projectId);
  const { mutate } = decide;
  const actionByApplication = useMemo(() => {
    const map = new Map<number, number>();
    for (const action of actions.data?.rows ?? []) {
      if (action.kind === 'auto_hire' && action.status === 'applied') map.set(action.application_id, action.id);
    }
    return map;
  }, [actions.data]);

  return useCallback(
    (application: Application) => {
      const actionId = application.meta?.['on_probation'] === true ? actionByApplication.get(Number(application.id)) : undefined;
      if (actionId === undefined) return undefined;
      return () =>
        mutate(
          { actionId, decision: 'undo', confirmNested: false },
          { onSuccess: () => void client.invalidateQueries({ queryKey: getListApplicationsQueryKey(projectId) }) },
        );
    },
    [actionByApplication, mutate, client, projectId],
  );
}
