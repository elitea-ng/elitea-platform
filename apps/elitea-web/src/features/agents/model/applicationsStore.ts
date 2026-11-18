import { create, type StoreApi, type UseBoundStore } from 'zustand';

/**
 * Port of `apps/elitea-ui/src/slices/applications.js` — the baseline's
 * Redux `applications` slice. This app has no Redux (`@reduxjs/toolkit` is
 * not a dependency — see `package.json`); `zustand` is the established
 * substitute (`widgets/app-shell/model/navBlocker.store.ts`,
 * `widgets/sidebar/model/sidebarCollapsed.store.ts`,
 * `features/credentials/model/useCredentialValidation.ts`).
 *
 * **What ported, and why only 3 of the baseline's 6 state slots:**
 *
 * - `isSaving`, `shouldRefetchDetails`, `versionValidationInfo` — genuine
 *   cross-component CLIENT state with no other home. `isSaving` is
 *   `dispatch(appActions.setIsSaving(isSaving))`'d by the baseline's
 *   `useSaveVersion.js:182-185` on every render (consumed elsewhere in the
 *   old app, outside this unit's owned files, by whatever renders a global
 *   "saving…" indicator); `versionValidationInfo` is written by
 *   `useValidateApplicationVersion.js`'s `extractValidationInfo` and read by
 *   its own `useToolsValidationInfo`/`useToolValidationInfo` (both owned by
 *   this unit — ported onto this store in `useValidateApplicationVersion.ts`).
 *
 * - `currentApplication`, `toolkitSchemas`, `mcpSchemas`,
 *   `configurationsAsSchema` — the baseline's `extraReducers` block
 *   (`slices/applications.js:37-89`), which mirrors 4 RTK Query
 *   `matchFulfilled` payloads into this slice purely so components could
 *   read them via `useSelector` instead of the query hook directly. TanStack
 *   Query removes the reason for that mirror entirely: any component that
 *   needs `applicationDetails`/`toolkitTypes`/`getAvailableConfigurationsType`
 *   data calls the real generated query hook for it directly (the cache IS
 *   the store) — mirroring it into a SECOND, hand-synchronised store would
 *   be duplicate, driftable state with no benefit. Not ported; no consumer
 *   inside this unit's owned files reads any of the 4 (confirmed by reading
 *   every owned file in full — `useCreateConfiguration.jsx` takes
 *   `configurationsAsSchema` as a caller-supplied PARAMETER, not from this
 *   slice, so the accumulation reducer has no consumer to preserve either).
 *
 * Lazy-singleton factory pattern (R-S2: "No store may be created at module
 * scope in a file that is also imported by `app/`") — see
 * `widgets/app-shell/model/navBlocker.store.ts`'s header for the full
 * rationale; mirrored verbatim here.
 */

/** One `versionValidationInfo[key]` entry — see `useValidateApplicationVersion.ts`'s own doc comment for why this is (and, on the real backend, always will be) an empty array today. */
export type VersionValidationEntry = Readonly<Record<string, unknown>>;

interface ApplicationsState {
  readonly isSaving: boolean;
  readonly shouldRefetchDetails: boolean;
  /** #1997 — the ⋮ menu's "Compare versions" item opens the version bar's compare modal through this flag. */
  readonly compareVersionsOpen: boolean;
  readonly versionValidationInfo: Readonly<Record<string, readonly VersionValidationEntry[]>>;
  readonly setIsSaving: (isSaving: boolean) => void;
  readonly setShouldRefetchDetails: (shouldRefetch: boolean) => void;
  readonly setCompareVersionsOpen: (open: boolean) => void;
  readonly setVersionValidationInfo: (key: string, info: readonly VersionValidationEntry[]) => void;
}

type ApplicationsStore = UseBoundStore<StoreApi<ApplicationsState>>;

export function createApplicationsStore(): ApplicationsStore {
  return create<ApplicationsState>((set) => ({
    isSaving: false,
    shouldRefetchDetails: false,
    compareVersionsOpen: false,
    versionValidationInfo: {},
    setIsSaving: (isSaving) => set({ isSaving }),
    setShouldRefetchDetails: (shouldRefetchDetails) => set({ shouldRefetchDetails }),
    setCompareVersionsOpen: (compareVersionsOpen) => set({ compareVersionsOpen }),
    setVersionValidationInfo: (key, info) =>
      set((state) => ({ versionValidationInfo: { ...state.versionValidationInfo, [key]: info } })),
  }));
}

let instance: ApplicationsStore | undefined;

function resolveStore(): ApplicationsStore {
  instance ??= createApplicationsStore();
  return instance;
}

function useApplicationsStoreHook<T>(selector: (state: ApplicationsState) => T): T {
  return resolveStore()(selector);
}

/** @public The lazily-constructed singleton, exposed with the same hook + getState/setState surface every call site (and every test) already uses (`widgets/app-shell/model/navBlocker.store.ts`'s own convention). */
export const useApplicationsStore = Object.assign(useApplicationsStoreHook, {
  getState: (): ApplicationsState => resolveStore().getState(),
  setState: (partial: Partial<ApplicationsState>): void => resolveStore().setState(partial),
});

/**
 * `slices/applications.js:29-35`'s `setVersionValidationInfo` action's own
 * key shape (`${projectId}_${applicationId}_${versionId}`) — duplicated
 * (not imported) from `entities/application-form/model/validationStatus.ts`'s
 * private `buildApplicationValidationKey`, which is intra-slice-only there
 * (not re-exported from that entity's `index.ts`) — `no-sideways-entities`
 * gives this file no legal way to reach it even if it were.
 */
export function buildVersionValidationKey(
  projectId: string | undefined,
  applicationId: number | string | undefined,
  versionId: number | string | undefined,
): string {
  return `${String(projectId)}_${String(applicationId)}_${String(versionId)}`;
}
