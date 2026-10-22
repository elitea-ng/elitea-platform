/**
 * useConfigurationsBySection — fetches configurations for all sections in parallel.
 * Ported from `apps/elitea-ui/src/hooks/credentials/useMultiSectionConfigurations.js`.
 *
 * DEFECT, fixed here: the combiner read only `q.data?.items` and put `[]` in
 * place of anything else. It also had no error channel. Seven failed section
 * queries therefore produced the same all-empty record as seven empty ones.
 * A 403 or a 500 therefore reached the page as "you have no LLM
 * configurations".
 * A user with a working credential saw it disappear. The `enabled: !!projectId`
 * guard was a second route into the same false-empty state: with no project id
 * nothing fetches, `isFetching` is false, and the record read as a success.
 *
 * The hook now returns `data: null` plus the first `error` when any section
 * fails, and stays in the loading state while the project id is unresolved.
 * The page already branches on a falsy `data`, so a null stops the empty-state
 * render. Do not go back to omitting the failed section instead: every
 * consumer reads `configurationsBySection['llm'] ?? []`, so an absent key
 * renders exactly like an empty one.
 */
import { useCallback, useMemo } from 'react';

import {
  getConfigurationsList,
  type ConfigurationItem,
} from '@/shared/api/configurationsApi';
import { useQueries } from '@tanstack/react-query';

/* ── section constants ──────────────────────────────────────────────────── */

const SECTIONS = [
  'llm',
  'embedding',
  'image_generation',
  'asr',
  'tts',
  'ai_credentials',
] as const;

type Section = (typeof SECTIONS)[number];

/** Map of section-name → flat list of configurations. */
type ConfigurationsBySection = Record<Section, ConfigurationItem[]>;

export interface ConfigurationsBySectionResult {
  data: ConfigurationsBySection | null;
  isLoading: boolean;
  /** The first section error. `null` when every section resolved. */
  error: unknown;
  refetch: () => void;
}

/* ── hook ───────────────────────────────────────────────────────────────── */

/**
 * Fetch configurations for all sections in parallel.
 * Returns an object keyed by section name, with each value being the
 * flat list of configurations (no pagination — `pageSize: 200` covers
 * the expected scale).
 */
export function useConfigurationsBySection(projectId: string): ConfigurationsBySectionResult {
  /* Fire one query per section in parallel — top-level useQueries call. */
  const queries = useQueries({
    queries: useMemo(() => {
      const base = {
        projectId,
        includeShared: true,
        pageSize: 200,
      };
      return SECTIONS.map((section) => ({
        queryKey: ['settings', 'configurations', projectId, section],
        queryFn: () =>
          getConfigurationsList({ ...base, section }),
        enabled: !!projectId,
      }));
    }, [projectId]),
  });

  /* Refetch the seven section queries only.
     Deliberately NOT `queryClient.invalidateQueries({ queryKey: ['settings',
     'configurations'] })`: that prefix is shared with
     `features/credentials/api/useConfigurations.ts`, so it would also refetch
     an unrelated screen's cache. */
  const refetch = useCallback(() => {
    queries.forEach((query) => {
      void query.refetch();
    });
  }, [queries]);

  /* Combine results */
  const result = useMemo((): Omit<ConfigurationsBySectionResult, 'refetch'> => {
    // No project id yet: nothing has been asked for, so this is still loading.
    // Reporting an empty record here would read as "this project has none".
    if (!projectId) return { data: null, isLoading: true, error: null };

    if (queries.some((q) => q.isFetching)) return { data: null, isLoading: true, error: null };

    const firstError = queries.find((q) => q.isError)?.error;
    if (firstError !== undefined) return { data: null, isLoading: false, error: firstError };

    const data: ConfigurationsBySection = {} as ConfigurationsBySection;
    SECTIONS.forEach((section, index) => {
      data[section] = queries[index]?.data?.items ?? [];
    });
    return { data, isLoading: false, error: null };
  }, [queries, projectId]);

  return { ...result, refetch };
}
