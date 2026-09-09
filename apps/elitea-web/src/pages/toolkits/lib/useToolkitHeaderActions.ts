/**
 * The toolkit edit page's HEADER actions: the Export/Delete permission gate
 * and the kebab's one real item, Copy link.
 *
 * Both used to live in `../EditToolkit.tsx`. They moved here unchanged when
 * that page gained its Save/Cancel control and went over the §3.5 400-line
 * budget — same "split for the budget, nothing else" move the page already
 * made for `IndexesTabPanel`. Their reasoning is preserved verbatim below.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { handleCopy } from '@/shared/lib/clipboard';
import { PERMISSIONS } from '@/shared/lib/permissions';
import type { ControlsDropdownItem } from '@/shared/ui/ControlsDropdown';

interface ToolkitActionPermissions {
  readonly canExport: boolean;
  readonly canDelete: boolean;
}

/**
 * Regression fix (parity-review finding R2): the baseline's
 * `ToolkitsControls.jsx` (lines 55-69) builds its Export/Delete kebab-menu
 * items with `disabled: !checkPermission(PERMISSIONS.applications.export) ||
 * !checkPermission(PERMISSIONS.toolkits.export)` (and the analogous
 * `applications.delete`/`toolkits.delete` pair) via `useCheckPermission()`,
 * computed fresh on every render. `DeleteToolkitButton`/`ExportToolkitButton`
 * (`features/toolkits/ui/*.tsx`, both outside this cluster's file scope)
 * already accept a `disabled` prop (defaulting to `false`) but their own doc
 * comments disclose "no `useCheckPermission`/`validatePermission`… port"
 * exists internally — this page (the only real caller) is where that gate
 * belongs instead. Local, not a shared hook: same "duplication is two
 * lines, not worth threading a new shared primitive through for" reasoning
 * `features/agents/lib/useHasPermission.ts`'s own doc comment gives for the
 * identical `usePermissionList` + `Set` composition (`no-upward-from-features`
 * bars THAT file from being reached from `pages/` anyway — a `features/`
 * slice's internals are off-limits to `pages/` regardless of which slice).
 */
export function useToolkitActionPermissions(projectId: string | undefined): ToolkitActionPermissions {
  const query = usePermissionList(projectId ?? '', { query: { enabled: projectId !== undefined } });

  return useMemo(() => {
    // `query.data.data`'s declared type includes the error-envelope variant —
    // never actually reachable here, since `eliteaFetch` throws instead of
    // resolving with it (mutator.ts's §3.6 unwrap contract).
    const list = query.data?.data as Permission[] | undefined;
    const granted = new Set((list ?? []).filter((entry) => entry.enabled).map((entry) => entry.name));
    return {
      canExport: granted.has(PERMISSIONS.applications.export) && granted.has(PERMISSIONS.toolkits.export),
      canDelete: granted.has(PERMISSIONS.applications.delete) && granted.has(PERMISSIONS.toolkits.delete),
    };
  }, [query.data]);
}

const COPIED_LABEL_DURATION_MS = 2500;

/**
 * Regression fix (parity-review finding R3, partial): the baseline's
 * `ToolkitsControls.jsx` `rightToolbar` renders a kebab dropdown with
 * Pin/Copy-Link/Fork/Export/Delete items, built by `usePinMenu`/
 * `useCopyLinkMenu`/`useForkEntityMenu`/`useDeleteToolkitMenu`/
 * `useExportToolkitMenu`. Of those five, ONLY copy-link is genuinely
 * buildable here: no `usePin`/`useToolkitFork`-equivalent endpoint wrapper
 * exists anywhere in this app (grepped: zero hits), and forking additionally
 * opens the baseline's `import-wizard` modal, which has no port anywhere
 * either — see the `EditToolkit` doc comment below for the full disclosure
 * of what remains unbuilt (Pin/Fork/the public-view Authors indicator).
 * Copy-link needs neither: `@/shared/lib/clipboard`'s real `handleCopy`
 * (the SAME primitive every other copy-to-clipboard call site in this app
 * already uses) plus the current page's own URL is the entire feature.
 * `window.location.href` (not the baseline's unported `useProjectEntityLink`)
 * is a disclosed simplification — THIS page's URL already IS the toolkit's
 * own detail-page URL.
 */
export function useCopyLinkMenuItem(): ControlsDropdownItem[] {
  const [copied, setCopied] = useState(false);
  const timeoutRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(() => () => clearTimeout(timeoutRef.current), []);

  const onClick = useCallback(() => {
    void handleCopy(window.location.href);
    setCopied(true);
    clearTimeout(timeoutRef.current);
    timeoutRef.current = setTimeout(() => setCopied(false), COPIED_LABEL_DURATION_MS);
  }, []);

  const label = copied
    ? t('pages.toolkits.editToolkit.copyLinkCopied', 'Copied!')
    : t('pages.toolkits.editToolkit.copyLink', 'Copy link');
  return [{ key: 'copy-link', label, onClick }];
}
