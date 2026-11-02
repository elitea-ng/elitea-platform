/**
 * Whether the signed-in person may open the admin portal (`lib/adminPortal.ts`
 * says what that means). Read once per session and kept: an administration
 * role changes rarely, and the admin console checks again on arrival.
 *
 * A background request: its 401 must not start a re-sign-in
 * (`shared/api/http.ts`'s `background`), and a failure reads as "no link".
 */
import { useQuery } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { getAdministrationPermissionListUrl, getAdministrationPermissionListQueryKey } from '@/shared/api/generated/auth/auth';

import { holdsAdministrationPermission } from '../lib/adminPortal';

const FIVE_MINUTES = 5 * 60_000;

export function useAdminAccess(): boolean {
  const query = useQuery({
    queryKey: getAdministrationPermissionListQueryKey(),
    queryFn: ({ signal }) =>
      eliteaFetch<{ data: unknown }>(getAdministrationPermissionListUrl(), { method: 'GET', signal }, { background: true }),
    staleTime: FIVE_MINUTES,
    retry: false,
  });
  return holdsAdministrationPermission(query.data?.data);
}
