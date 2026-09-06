/**
 * Ported from `apps/elitea-ui/src/[fsd]/shared/lib/hooks/
 * useEnvironmentSettingByKey.hooks.js` (36 lines).
 *
 * This hook reads an environment setting from the app config by key.
 * The old-app version calls RTK Query's `useGetConfigurationsListQuery` to
 * fetch `environment_settings` for the public project. This new-app version
 * reads from the new app's runtime config (`shared/config`) — a closed,
 * build-time set of env-derived keys — which is the existing new-app pattern
 * for reading config/env settings (§7.1 / `shared/config/get-config.ts`).
 *
 * The convenience wrappers (`useSystemSenderName`, `useErrorToastDuration`)
 * mirror the old-app equivalents; `useErrorToastDuration` defaults to
 * `DEFAULT_TOAST_DURATION` when the env key is absent.
 */
import { useMemo } from 'react';

import { getConfig } from '@/shared/config';

/** Keys the old-app `environment_settings` section exposes. */
export const ENVIRONMENT_KEYS = {
  SYSTEM_SENDER_NAME: 'system_sender_name',
  ERROR_TOAST_DURATION: 'error_toast_duration',
} as const;

/** Default toast duration (seconds), old-app `constants.js` fallback. */
export const DEFAULT_TOAST_DURATION = 5000;

/** Default system sender name, old-app `constants.js` fallback. */
/**
 * `apps/elitea-ui/src/common/constants.js:89` — `'Elitea'`, not `'Assistant'`.
 *
 * This is the name every assistant transcript row is captioned with when a
 * deployment states no `system_sender_name`, which is the default. It said
 * `'Assistant'` here alone; the app's two OTHER copies of the same constant
 * (`shared/lib/copy.ts`, `entities/participant/model/selectors.ts`) already
 * carry the baseline's `'Elitea'`, so the three disagreed.
 */
export const DEFAULT_PARTICIPANT_NAME = 'Elitea';

/**
 * Result shape returned by `useEnvironmentSettingByKey`.
 * Mirrors the old-app return value (`{ value, isLoading, isFetching, error }`).
 *
 * `isLoading`/`isFetching` are derived from `getConfig()` which is synchronous
 * (reads from import-time sources). They default to `false` — set to `true`
 * when the new app wires this hook to a real server-side query.
 * `error` is always `null` until a server-side query is wired in.
 */
export interface UseEnvironmentSettingByKeyResult {
  readonly value: string | null;
  readonly isLoading: boolean;
  readonly isFetching: boolean;
  readonly error: unknown;
}

/**
 * `useEnvironmentSettingByKey` — reads an environment setting from the app
 * config by key.
 *
 * The old-app version fetches from the `/configurations/configurations/{projectId}`
 * API endpoint (section: `environment_settings`). The new-app version reads
 * from `shared/config` (build-time Vite env vars). The return shape is
 * identical so that callers (`useSystemSenderName`, etc.) require no changes.
 *
 * @param key — the environment setting key to read. When omitted/empty,
 *              the hook returns all-null defaults (old-app `skip: !key` pattern).
 */
export function useEnvironmentSettingByKey(
  key: string | null | undefined,
): UseEnvironmentSettingByKeyResult {
  const { config } = useMemo(() => {
    const result = getConfig();
    return {
      config: result.status === 'ok' ? result.config : null,
      missing: result.status === 'missing' ? result.missing : [],
      reasons: result.status === 'missing' ? result.reasons : {},
    };
  }, []);

  const value = useMemo(() => {
    if (!key || !config) return null;
    // eslint-disable-next-line @typescript-eslint/no-unsafe-member-access -- dynamic key on a frozen object
    const raw = (config as Record<string, unknown>)[key];
    return typeof raw === 'string' ? raw : null;
  }, [config, key]);

  return {
    value,
    isLoading: false,
    isFetching: false,
    error: null,
  };
}

/**
 * `useSystemSenderName` — convenience wrapper over `useEnvironmentSettingByKey`
 * for the `system_sender_name` key. Falls back to `DEFAULT_PARTICIPANT_NAME`
 * when the env key is absent.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/shared/lib/hooks/
 * useEnvironmentSettingByKey.hooks.js:26-28`.
 */
export function useSystemSenderName(): string {
  const { value } = useEnvironmentSettingByKey(ENVIRONMENT_KEYS.SYSTEM_SENDER_NAME);
  return value ?? DEFAULT_PARTICIPANT_NAME;
}

/**
 * `useErrorToastDuration` — convenience wrapper over
 * `useEnvironmentSettingByKey` for the `error_toast_duration` key.
 * Falls back to `DEFAULT_TOAST_DURATION` when the env key is absent.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/shared/lib/hooks/
 * useEnvironmentSettingByKey.hooks.js:31-34`.
 */
export function useErrorToastDuration(): number {
  const { value } = useEnvironmentSettingByKey(ENVIRONMENT_KEYS.ERROR_TOAST_DURATION);
  return value ? parseInt(value, 10) : DEFAULT_TOAST_DURATION;
}
