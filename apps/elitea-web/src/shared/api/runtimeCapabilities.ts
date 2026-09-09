import { useGetRuntimeCapabilities } from '@/shared/api/generated/toolkits/toolkits';
import type { RuntimeCapabilities } from '@/shared/api/generated/model';

/**
 * Which worker this deployment runs, and what it cannot do (#865, #866).
 *
 * Kept `shared/`, not feature-local: `features/agents` (the internal-tool
 * toggles, `features/agents/lib/internalTools.ts`) and `features/toolkits`
 * (the toolkit type picker, `ToolkitTypeSelector.tsx`) both need it, and
 * `no-sideways-features` forbids one feature importing the other's hooks —
 * the same reason `useIsMcpVisible` documents for staying feature-local
 * versus this module's cross-feature placement.
 *
 * Before this endpoint existed, neither surface had any signal for which
 * worker was deployed at all — `internalTools.ts`'s own `pyodide` entry
 * documented the gap directly ("the app has no signal anywhere for which
 * worker a deployment is running"). A toolkit type the worker could not
 * build was OMITTED from the type picker with no explanation
 * (`toolkitTypeMenuEntries`'s `metadata['hidden'] !== true` filter), and an
 * internal tool the worker could not run answered as if nothing had
 * happened. This hook is what both surfaces now read instead.
 */
export function useRuntimeCapabilities(): RuntimeCapabilities | undefined {
  const query = useGetRuntimeCapabilities();
  // Same unwrap contract useIsMcpVisible.ts documents: `eliteaFetch` throws
  // rather than resolving with the error-envelope variant `.data.data`'s
  // declared type still carries, so the cast below is never reached on the
  // error branch in practice.
  return query.data?.data as RuntimeCapabilities | undefined;
}

/**
 * Whether the CONFIGURED worker runs one of the six toggleable internal
 * chat tools (`features/agents/lib/internalTools.ts`'s `INTERNAL_TOOLS_LIST`
 * names) for real, per #866.
 *
 * Returns `true` — available — for a tool name this deployment's
 * capabilities response does not mention at all (`attachments`, `pyodide`,
 * any tool this endpoint has not been taught yet) and while the query has
 * not resolved, matching this app's existing "unset means offered, not
 * withheld" convention (`ToolkitCapabilitySource`'s own nil case, the
 * generated `metadata.hidden` default) — never disable a toggle on missing
 * data.
 */
export function isInternalToolAvailable(
  capabilities: RuntimeCapabilities | undefined,
  toolName: string,
): boolean {
  const available = capabilities?.internal_tools?.[toolName];
  return available ?? true;
}

/** The set of catalogued toolkit type keys the configured worker cannot build (#865). Empty — never withholding a type — while the query has not resolved. */
export function hiddenToolkitTypes(capabilities: RuntimeCapabilities | undefined): ReadonlySet<string> {
  return new Set(capabilities?.hidden_toolkit_types ?? []);
}
