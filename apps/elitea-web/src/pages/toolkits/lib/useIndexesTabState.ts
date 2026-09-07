/**
 * Everything `EditToolkit` needs to decide about the Indexes tab, in one
 * hook — split out of that component purely to keep it under the §3.5
 * complexity budget (12), exactly as `useSaveToolkitMutation` in that same
 * file already was. The branching is unchanged, it just does not count
 * against the component.
 */
import { useMemo } from 'react';

import { useIndexesTabVisibility } from '@/features/toolkits';

import type { EditToolDetail } from './toolkitFormTypes';

export interface UseIndexesTabStateParams {
  readonly isMCP: boolean;
  /** The fetched, saved toolkit row. */
  readonly detail: { readonly type?: string | undefined; readonly settings?: Record<string, unknown> | undefined } | undefined;
  /** The live, possibly-unsaved editor state. */
  readonly editToolDetail: EditToolDetail | null;
  /** The currently selected tab index. */
  readonly tab: number;
}

export interface IndexesTabState {
  /** True when the tab must not be offered at all — no label, no panel. */
  readonly hidden: boolean;
  /** The `IndexesToolsEnum` members this toolkit has actually selected. */
  readonly selectedIndexTools: readonly string[];
  /**
   * The worker-capability verdict off the served type schema — set when the
   * worker cannot run this toolkit type at all. `IndexesTab` renders it in
   * place of the create form; see `indexesTabVisibility.ts`.
   */
  readonly unavailableReason?: string | undefined;
  /** `tab`, collapsed to Configuration whenever the Indexes tab is not offered. */
  readonly activeTab: number;
  /** The toolkit's `{type, settings}`, as the baseline's Formik `values` reached `IndexesContainer`. */
  readonly toolkitValues: Record<string, unknown>;
}

export function useIndexesTabState(params: UseIndexesTabStateParams): IndexesTabState {
  const { isMCP, detail, editToolDetail, tab } = params;

  const visibility = useIndexesTabVisibility({
    isMCP,
    toolkitType: detail?.type,
    selectedTools: editToolDetail?.settings?.['selected_tools'],
  });

  /**
   * Read off the LIVE editable state with the fetched detail as the
   * fallback, so an unsaved tool selection reaches the run payload the same
   * way the baseline's shared Formik context did.
   */
  const toolkitValues = useMemo<Record<string, unknown>>(
    () => ({
      type: editToolDetail?.type ?? detail?.type,
      settings: editToolDetail?.settings ?? detail?.settings,
    }),
    [editToolDetail, detail],
  );

  return {
    hidden: visibility.hidden,
    selectedIndexTools: visibility.selectedIndexTools,
    // Key omitted, not set to `undefined`: `exactOptionalPropertyTypes`.
    ...(visibility.unavailableReason === undefined ? {} : { unavailableReason: visibility.unavailableReason }),
    // The tab index is only stable while the Indexes tab exists; if it
    // disappears under a selection change (e.g. the last index tool is
    // deselected), fall back to Configuration rather than render nothing.
    activeTab: visibility.hidden ? 0 : tab,
    toolkitValues,
  };
}
