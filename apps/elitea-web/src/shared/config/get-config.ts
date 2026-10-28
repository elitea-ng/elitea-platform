import {
  CONFIG_KEYS,
  ConfigSchema,
  type Config,
  type ConfigKey,
  type RequiredConfigKey,
} from './schema';
import { readConfigKey } from './sources';

/**
 * §3.6 — errors are values at the boundary: a missing or invalid runtime
 * config is a `{ status: 'missing' }` result, never a throw.
 *
 * Import-time claim, stated accurately: importing this module (or the barrel)
 * builds the zod schema and the source list — pure object construction — and
 * performs NO side effects, NO I/O, NO source reads and NO store creation
 * (R-S2 discipline). Nothing is resolved until the app layer calls
 * getConfig(), which is old-App.jsx:11 parity for the MISSING_ENVS branch.
 */
export type ConfigResult =
  | { readonly status: 'ok'; readonly config: Config }
  | {
      readonly status: 'missing';
      /** Required keys that are absent or invalid, in C7 contract order. */
      readonly missing: readonly RequiredConfigKey[];
      /** Per-key diagnostics: why each unusable key is unusable. */
      readonly reasons: Readonly<Record<string, string>>;
    };

function resolveConfig(): ConfigResult {
  // `unknown`, not `string`: `allow_project_own_llms` is deliberately
  // unparsed passthrough (schema.ts) — it can hold any raw source value.
  const values: Partial<Record<ConfigKey, unknown>> = {};
  const reasons: Record<string, string> = {};

  for (const key of CONFIG_KEYS) {
    const hit = readConfigKey(key);
    if (hit === undefined) {
      continue;
    }
    if (hit.value === undefined) {
      // getEnvVar parity: a source can define a key as `undefined`; the
      // definition still wins the C6 race but contributes no value.
      continue;
    }
    const parsed = ConfigSchema.shape[key].safeParse(hit.value);
    if (parsed.success) {
      values[key] = parsed.data;
    } else {
      // Zod-invalid ⇒ treated as missing (C7 contract). Deliberate deviation
      // from the old app, which passed any non-null junk value downstream.
      reasons[key] =
        `invalid value from ${hit.source}: ` +
        parsed.error.issues.map((issue) => issue.message).join('; ');
    }
  }

  const parsed = ConfigSchema.safeParse(values);
  if (parsed.success) {
    return Object.freeze({ status: 'ok' as const, config: Object.freeze(parsed.data) });
  }

  // `values` only ever holds field-validated entries for known keys, so the
  // only possible issues are required keys that ended up without a value.
  const missing = parsed.error.issues.map(
    (issue) => String(issue.path[0]) as RequiredConfigKey,
  );
  // Deterministic C7 contract order, independent of zod's issue ordering.
  missing.sort((a, b) => CONFIG_KEYS.indexOf(a) - CONFIG_KEYS.indexOf(b));
  for (const key of missing) {
    reasons[key] ??= 'not defined in any config source';
  }
  return Object.freeze({
    status: 'missing' as const,
    missing: Object.freeze(missing),
    reasons: Object.freeze(reasons),
  });
}

let cached: ConfigResult | undefined;

/**
 * Resolve the runtime config (C6/C7). Pure factory: no module-scope work, no
 * side effects beyond the memo below.
 *
 * Memoization/invalidation story: in a browser the C6 sources are immutable
 * for the life of a page — /app/config.js is executed before the bundle
 * (index.html loads it first), and build-time env cannot change at runtime —
 * so a resolved config is cached and every later call returns the same frozen
 * result. Only an `ok` result is cached: the desktop build publishes
 * `elitea_ui_config` at sign-in (entries/desktop/launchApp.tsx), and anything
 * the pre-sign-in shell resolves earlier (BrandDocumentHead's basename read)
 * must not pin a `missing` result that would show MissingEnvPage once the
 * config exists. There is deliberately NO production invalidation of an `ok`
 * result; tests use resetConfigForTests() (not re-exported from the public
 * barrel).
 */
export function getConfig(): ConfigResult {
  if (cached !== undefined) return cached;
  const result = resolveConfig();
  if (result.status === 'ok') cached = result;
  return result;
}

/** Test-only memo reset — kept off the public surface (see index.ts). */
export function resetConfigForTests(): void {
  cached = undefined;
}
