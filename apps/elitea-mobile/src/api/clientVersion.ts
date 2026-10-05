import * as Application from 'expo-application';
import Constants from 'expo-constants';

/** Header the server compares with its `min_client_version` (ADR-0025). */
export const CLIENT_VERSION_HEADER = 'X-Client-Version';

const SEMVER = /^\d+\.\d+\.\d+$/;

/**
 * Picks the plain `MAJOR.MINOR.PATCH` version to send: the installed native
 * version when it is a plain semver, else the version in app.config.ts, else
 * `0.0.0` (which any server minimum rejects, failing safe).
 */
export function resolveClientVersion(
  nativeVersion: string | null | undefined,
  configVersion: string | null | undefined,
): string {
  if (nativeVersion && SEMVER.test(nativeVersion)) return nativeVersion;
  if (configVersion && SEMVER.test(configVersion)) return configVersion;
  return '0.0.0';
}

let cached: string | null = null;

export function getClientVersion(): string {
  cached ??= resolveClientVersion(Application.nativeApplicationVersion, Constants.expoConfig?.version);
  return cached;
}
