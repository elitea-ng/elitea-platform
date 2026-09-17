import { useMemo } from 'react';

import { useListToolkitInstances } from '@/shared/api/generated/toolkits/toolkits';
import type { ToolkitInstance } from '@/shared/api/generated/model';
import { unwrapList } from '@/shared/api/unwrap';

/**
 * Page-local duplicate of `features/toolkits/api/toolkits.ts`'s
 * `useToolkitDetail`/`useToolkitsList` (same `GET /elitea_core/tools/
 * prompt_lib/{projectId}` real endpoint, same "no GET-single endpoint
 * exists — find the row inside the real list client-side" derivation — see
 * that file's own module doc comment for the full, exhaustively-verified
 * backend-gap inventory).
 *
 * NOT an import of that hook: `no-deep-slice-import` forbids `pages/`
 * reaching a `features/` slice's internals directly, and `features/
 * toolkits`' public `index.ts` does not export `useToolkitDetail` — its own
 * budget is already at the §3.5 20-symbol ceiling with the four pieces
 * `pages/toolkits/CreateToolkit.tsx`/`EditToolkit.tsx` need more (`ToolkitForm`/
 * `ToolkitTypeSelector`/`CreateToolkitToolTabBar`/`ConfigurationTab` — see
 * `features/toolkits/index.ts`'s own doc comment). `MAX_DETAIL_LOOKUP_PAGE_SIZE`
 * carries the identical "pragmatic single-page fetch, not real pagination"
 * caveat that file's own doc comment discloses.
 */
/**
 * 100, the LARGEST page the server actually serves.
 *
 * It was 200, and that number was worse than a smaller one: the handler
 * refuses an out-of-range limit by falling back to its DEFAULT rather than
 * clamping — `if limit < 1 || limit > 100 { limit = 20 }`
 * (`internal/api/v2/toolkits/handler.go`'s `List`). So asking for 200 got
 * TWENTY rows, and this hook finds the toolkit by scanning the page it
 * received: every toolkit past the twentieth opened an editor with no
 * fields, no Tools section, no Indexes tab and no error anywhere — the
 * detail was simply absent. Measured on a project with 30 toolkits.
 *
 * This is still a single-page fetch, not real pagination — the disclosed
 * caveat this file's header already carries — but the page is now the one
 * the server will give.
 */
const MAX_DETAIL_LOOKUP_PAGE_SIZE = 100;

export interface UseToolkitDetailResult {
  readonly detail: ToolkitInstance | undefined;
  readonly isFetching: boolean;
  readonly isError: boolean;
}

export function useToolkitDetail(projectId: string | undefined, toolkitId: string | undefined): UseToolkitDetailResult {
  const query = useListToolkitInstances(
    projectId ?? '',
    { limit: MAX_DETAIL_LOOKUP_PAGE_SIZE, offset: 0 },
    { query: { enabled: projectId !== undefined && toolkitId !== undefined } },
  );
  // R-A6 (#132): one helper unwraps the envelope + body shape, instead of a
  // cast that asserts `{rows,total}` and renders an empty list when it is wrong.
  const rows = useMemo(() => unwrapList<ToolkitInstance>(query.data, 'listToolkitInstances'), [query.data]);
  const detail = useMemo(() => rows.find((row) => row.id === toolkitId), [rows, toolkitId]);

  return { detail, isFetching: query.isFetching, isError: query.isError };
}
