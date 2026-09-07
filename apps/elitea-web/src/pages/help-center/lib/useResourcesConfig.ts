/**
 * useResourcesConfig — the Help Center's admin-configurable data: per-card
 * enabled flags and links, plus the version label.
 *
 * ## This was the blocked half of issue #26, and unit A14 unblocked it
 *
 * Until the admin Configuration port (issue #200) this hook made no network call
 * at all. The comment it carried was accurate about the SYMPTOM and wrong about
 * the cause: it read as an OpenAPI/orval gap, when in fact the endpoint behind it
 * did not work. `GET /admin/plugin_config_values/prompt_lib/resources` had a
 * route and answered 200 — with `max_file_size`, `max_context_length`,
 * `streaming_enabled` and a dozen other chat and upload limits, under no `values`
 * wrapper. It was a handler answering a different question than the page asked,
 * and no client that called it could have got a link out of it.
 *
 * It now serves what an administrator saved on Admin › Configuration ›
 * Resources, out of `centry.platform_config`
 * (`services/elitea-main/internal/api/v2/admin/config_values.go`). That is the
 * same route, the same section and the same public-read rule pylon has — pylon
 * exposes exactly one section to non-administrators, `_PUBLIC_SECTIONS =
 * {"resources"}` — so the contract is preserved rather than invented.
 *
 * ## It reads through the generated client (issue 36, item 7)
 *
 * This hook used to call `eliteaFetch` with a literal URL, because `v2.yaml`
 * described no admin-panel route and there was nothing for orval to generate.
 * The route IS described now — `getResourcesConfigValues` — so the URL and the
 * response type come from `shared/api/generated/resources`. The `useQuery`
 * around it stays hand-written: this hook owns a five-minute `staleTime` and a
 * fail-open default that orval's generated query options know nothing about.
 *
 * ## What is still empty, and honestly so
 *
 * `plugins` — the per-plugin version list in `ResourceVersionInfo`'s tooltip.
 * Its source is `GET /admin/system_info/prompt_lib`. In pylon that route reports
 * the versions of six named plugins, collected from the pylons that announced
 * themselves on the Arbiter bus in the last 60 seconds. This service loads no
 * plugins and has no such bus, so issue #219 made the route answer 501 with that
 * reason instead of the hardcoded `elitea_core`/`auth` map it used to invent.
 *
 * This hook therefore does NOT call it. A request whose only possible answer is
 * a refusal buys nothing and puts a 501 in the network log of every Help Center
 * visit. It returns an empty list, and `ResourceVersionInfo` keeps the tooltip
 * closed. The "Version: X (date)" label beside the icon is unaffected: it comes
 * from the administrator-owned resources section read below, which is real.
 *
 * Parity item API-178 asks this client to call `system_info`. Wire it up when the
 * service has a build version to report — see the `systemInfoUnavailable` comment
 * in `services/elitea-main/internal/api/v2/admin/handler.go` for what that needs.
 */
import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';

import { getResourcesConfigValues } from '@/shared/api/generated/resources/resources';
import { unwrapBody } from '@/shared/api/unwrap';

import { RESOURCE_CARD_CONFIGS } from './ResourceCardConfig';

/** A single plugin's name/version, as shown in `ResourceVersionInfo`'s tooltip. */
export interface ResourcesConfigPlugin {
  readonly name: string;
  readonly version?: string;
}

export interface ResourcesConfigResult {
  /** Raw admin config values, keyed by `*_enabled`/`*_links` (see `ResourceCardConfig.ts`). */
  readonly configValues: Record<string, unknown>;
  /** Pre-formatted "Version: X (date)" label — `''` when neither is configured. */
  readonly versionLabel: string;
  /** Per-plugin versions shown in `ResourceVersionInfo`'s tooltip. */
  readonly plugins: ReadonlyArray<ResourcesConfigPlugin>;
}

/**
 * Every card enabled, no links — what the page shows while the read is in
 * flight, and if it fails.
 *
 * Failing OPEN is deliberate here and is the opposite of the choice a permission
 * check makes. These flags decide whether a documentation card is visible; a
 * transient error that hid the Help Center's contents would be a worse outcome
 * than showing the cards with no links, which is exactly what an unconfigured
 * platform shows anyway.
 */
const DEFAULT_CONFIG_VALUES: Record<string, unknown> = Object.fromEntries(
  RESOURCE_CARD_CONFIGS.map((c): [string, boolean] => [c.enabledKey, true]),
);

/**
 * Builds the "Version: X (date)" label from the two values the administrator
 * sets on the Information card. Empty when neither is configured — a bare
 * "Version:" with nothing after it reads as a rendering bug.
 */
export function resourcesVersionLabel(configValues: Record<string, unknown>): string {
  const version = configValues.resources_information_version;
  const upgraded = configValues.resources_information_upgrade_date;
  const versionText = typeof version === 'string' ? version.trim() : '';
  const upgradedText = typeof upgraded === 'string' ? upgraded.trim() : '';
  if (versionText === '' && upgradedText === '') return '';
  if (upgradedText === '') return `Version: ${versionText}`;
  if (versionText === '') return `Last upgrade: ${upgradedText}`;
  return `Version: ${versionText} (${upgradedText})`;
}

/** Returns the Help Center's current admin-configured data. */
export function useResourcesConfig(): ResourcesConfigResult {
  const query = useQuery({
    queryKey: ['help-center', 'resources-config'],
    queryFn: async (): Promise<Record<string, unknown>> => {
      // The generated fetcher resolves `{data,status,headers}`; `unwrapBody` is
      // the one sanctioned peel (R-A6). Reading `.values` off the envelope
      // instead is the silent-empty-state defect of #132, and on this page it
      // would look exactly like the gap this change closes.
      const body = unwrapBody(await getResourcesConfigValues()) as
        | { values?: Record<string, unknown> }
        | undefined;
      return body?.values ?? {};
    },
    // These values change when an administrator edits them, which is rare, and
    // the page is opened often. Refetching on every mount would be a request per
    // visit for an answer that almost never differs.
    staleTime: 5 * 60 * 1000,
  });

  return useMemo((): ResourcesConfigResult => {
    const configValues = { ...DEFAULT_CONFIG_VALUES, ...query.data };
    return {
      configValues,
      versionLabel: resourcesVersionLabel(configValues),
      plugins: [],
    };
  }, [query.data]);
}
