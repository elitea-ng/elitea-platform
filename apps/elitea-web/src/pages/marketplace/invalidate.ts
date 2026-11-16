/**
 * After a Marketplace write (a hire, a version state, a feature flag) every
 * Marketplace read is stale: the grid, the shelves, the listing page and the
 * origin banner. The generated hooks key by URL, so this matches on the path.
 */
import type { QueryClient } from '@tanstack/react-query';

export function invalidateMarketplace(queryClient: QueryClient): void {
  void queryClient.invalidateQueries({
    predicate: (query) =>
      query.queryKey.some(
        (part) =>
          typeof part === 'string' && (part.includes('marketplace') || part.includes('/applications/prompt_lib')),
      ),
  });
}
