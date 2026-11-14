/**
 * The "on any" search-param scope (P1's manifest: `PARAM-062`..`PARAM-087`
 * minus `shared_chat`, which is chat-only). REPRODUCE.md choice #4: keys
 * read from shared components (e.g. `ProjectSelect.jsx`, `DataTable.jsx`,
 * `useIsFromSpecificPageHooks.jsx`) are attributed to `any` because they
 * are reachable from more than one screen.
 *
 * `query` is not in that manifest: it is the page toolbar's search text
 * (`widgets/page-header`'s `PageSearch`), shared by every list page and type
 * gallery, so it is shell-wide too.
 *
 * Mounted on `_shell/route.tsx` so every shell-wrapped route inherits these
 * 25 keys as part of its typed search (TanStack Router composes a leaf
 * route's search type from every ancestor's `validateSearch`). `auth-callback`
 * sits outside `_shell` and does not get this scope (parity: the old popup
 * page reads only `auth_state`).
 */
import { pickParams } from './params';

export const commonSearchSchema = pickParams(
  'author_id',
  'author_name',
  'bucket',
  'conversation',
  'create',
  'destTab',
  'entity_folder',
  'from',
  'history_run_id',
  'index_name',
  'isFromCreation',
  'message_id',
  'name',
  'project_id',
  'query',
  'return_url',
  'save_toolkit',
  'sort_by',
  'sort_order',
  'statuses',
  'tags[]',
  'toolkit_type',
  'tour',
  'view',
  'viewMode',
  'page_size',
);
