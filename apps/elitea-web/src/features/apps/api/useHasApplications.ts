import { useApplicationToolkits } from './useApplicationToolkits';

export interface HasApplicationsState {
  readonly hasApplications: boolean;
  readonly isLoading: boolean;
}

/**
 * Whether the project has an application: a toolkit instance of an
 * application type (see `useApplicationToolkits`). It read
 * `GET /elitea_core/applications` before, which lists agents and never held
 * a Wiki or an Inventory, so it answered `false` for a project that had
 * created one (#2113).
 */
export function useHasApplications(): HasApplicationsState {
  const { applications, isLoading } = useApplicationToolkits();
  return { hasApplications: applications.length > 0, isLoading };
}
