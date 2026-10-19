/**
 * One project's Send to cloud switch is read over two routes — the project's
 * own (`/elitea_core/project_cloud_executor/prompt_lib/{id}`, Settings ›
 * General) and the admin one (`/admin/project_cloud_executor/administration/{id}`,
 * Admin › Projects) — so the same state lives under two query keys. A save
 * through either route must land in both, or the other screen keeps showing
 * the old value until a reload (PR #1319 review).
 *
 * Both routes return the same `ProjectCloudExecutorSwitch` envelope, so the
 * saved response is written to both keys as is.
 */
import type { QueryClient } from '@tanstack/react-query';

import { getGetAdminProjectCloudExecutorQueryKey } from '@/shared/api/generated/admin/admin';
import { getGetProjectCloudExecutorQueryKey } from '@/shared/api/generated/settings/settings';

/** Every query key that caches project `projectId`'s switch. */
function cloudExecutorSwitchQueryKeys(projectId: string) {
  return [getGetProjectCloudExecutorQueryKey(projectId), getGetAdminProjectCloudExecutorQueryKey(projectId)] as const;
}

/** Store a save's response under every key that caches the project's switch. */
export function storeSavedCloudExecutorSwitch(queryClient: QueryClient, projectId: string, response: unknown): void {
  for (const key of cloudExecutorSwitchQueryKeys(projectId)) {
    queryClient.setQueryData(key, response);
  }
}
