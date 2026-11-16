/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 *
 * The `ToolTypes`/`ToolEvents`/`ToolInitialValues`/`toolIconStaticUrl`/
 * `ConfigurationMode`/`CONFIGURATION_VIEW_OPTIONS`/
 * `configurationDoesNotMatchAnything`/`ToolConfigurationForm`/
 * `toolkitValidationErrors`/`toolkitTypeMenuEntries` block below is the
 * Wave-2 promotion pass's Part 2 (+ part of Part 3) landing — see
 * `model/toolForm.ts`, `model/toolConfigurationMode.ts`,
 * `model/validationStatus.ts`, `model/toolMenu.ts` and
 * `ui/ToolConfigurationForm.tsx` for the full per-symbol provenance. 11 new
 * exports + the 8 that were already here = 19/20 — one slot of deliberate
 * headroom; several more pure helpers exist in those files but are NOT
 * re-exported here yet (intra-slice-only for now) because no concrete
 * cross-slice caller has landed to justify spending the budget on them —
 * see the promotion report for the full list of what was left off.
 *
 * `toolkitValidation` (Wave-2 unit C3): `api/useValidateToolkit.ts` moved
 * here from `features/agents` — it had zero real consumers there and needs
 * to be reachable from BOTH `features/agents` and `features/chat-input`
 * (see that file's own doc comment for the full why). Its 4 symbols
 * (`useValidateToolkit`, `useToolkitValidationInfo`, `buildToolkitValidationKey`,
 * `useToolkitValidationStore`) are bundled into ONE object export — the
 * established convention for spending a single budget slot on a
 * multi-symbol group (matches this file's own header note about pure
 * helpers left off individually) — bringing this barrel to the 20/20 cap
 * exactly.
 *
 * `toolkitTools` (#440). `api/toolkitToolsApi.ts` reads the two dynamic
 * tool-catalogue routes (`toolkit_available_tools`, `toolkit_discover_tools`).
 * Both `features/toolkits` and `features/pipelines` need them — seven tool
 * pickers between them — and `no-sideways-features` forbids either slice to
 * import the other, so `entities/` is the only legal home. Its 3 functions
 * take ONE slot under the bundling convention above.
 *
 * THE CAP DOES NOT MOVE. It stays 20. This barrel was already at 20/20, so
 * #440 retires one slot to pay for its own: `ToolConfigurationFormProps`.
 * That type has no importer outside this slice, and `ToolConfigurationForm`
 * stays exported, so no caller loses a capability.
 * #440 also keeps the four `toolkitToolsApi` types OFF this barrel, for the
 * same reason: no cross-slice caller reads them. Import them from the module
 * inside the slice. Retire a slot before adding a 21st.
 */
import {
  discoverToolkitTools,
  fetchAvailableTools,
  useToolkitTools,
} from './api/toolkitToolsApi';
import {
  buildToolkitValidationKey,
  useToolkitValidationInfo,
  useToolkitValidationStore,
  useValidateToolkit,
} from './api/useValidateToolkit';

export type { Toolkit, ToolkitAuthor, ToolkitPage, ToolkitTypeSchemaMap } from './model/types';
export { isMcpToolkit, isOnlineToolkit, sortToolkitsByName, toolkitDisplayName } from './model/selectors';

export { ToolEvents, ToolTypes, toolIconStaticUrl } from './model/toolForm';
export { ToolInitialValues } from './model/toolInitialValues';
export { CONFIGURATION_VIEW_OPTIONS, ConfigurationMode, configurationDoesNotMatchAnything } from './model/toolConfigurationMode';
export { toolkitValidationErrors } from './model/validationStatus';
export { toolkitTypeMenuEntries } from './model/toolMenu';
export { ToolConfigurationForm } from './ui/ToolConfigurationForm';

/** `api/useValidateToolkit.ts`'s 4 symbols, bundled — same one-slot-per-group convention as `entities/conversation`'s `conversationApi`. `useValidateToolkit`/`useToolkitValidationInfo` are the hooks; `useToolkitValidationStore` (the shared zustand store) and `buildToolkitValidationKey` (its key builder) are exposed for a sibling feature to read/derive from directly. */
export const toolkitValidation = {
  useValidateToolkit,
  useToolkitValidationInfo,
  useToolkitValidationStore,
  buildToolkitValidationKey,
} as const;

/** `api/toolkitToolsApi.ts`'s dynamic tool catalogue (#440), bundled into one slot — same convention as `toolkitValidation` above. `useToolkitTools` is the hook every picker uses; the two plain fetchers are exposed for a caller that needs the read outside React. */
export const toolkitTools = {
  useToolkitTools,
  fetchAvailableTools,
  discoverToolkitTools,
} as const;
