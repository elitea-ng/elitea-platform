import { useQuery, type QueryKey } from '@tanstack/react-query';
import { useSyncExternalStore } from 'react';

import { offlineStore } from '@/db/offlineStore';

const isOpen = () => offlineStore.status === 'open';

/** True while the encrypted offline store is open (after unlock, with retention > 0). */
export function useOfflineStoreOpen(): boolean {
  return useSyncExternalStore(offlineStore.subscribe, isOpen, isOpen);
}

/**
 * The copy of a screen's data saved on this device, read once the store is
 * open, for use as react-query placeholder data (DEF-R5: show the saved copy
 * first, then refresh from the workspace). Null when there is none. Kept under
 * the screen's own key, so it is dropped with its project and wiped with the
 * query cache at sign-out.
 */
export function useDeviceCopy<T>(key: QueryKey, enabled: boolean, read: () => Promise<T | null>): T | null {
  const open = useOfflineStoreOpen();
  const query = useQuery({
    queryKey: [...key, 'device-copy'],
    enabled: enabled && open,
    queryFn: async () => (await read()) ?? null,
    retry: false,
    // Only a placeholder until the workspace answers; never refetched on its own.
    staleTime: Infinity,
    gcTime: 60_000,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
  return query.data ?? null;
}
