/**
 * Agent Hub data hook — local port of the old Redux-based `useAgentHubData`.
 *
 * Strategy: fetch a page of published agents, then bucket it client-side by
 * category tag. Special buckets (Trending, My Liked) use their own targeted
 * requests, which the server now answers with real sorting and real
 * per-caller filtering.
 *
 * Every request goes through `eliteaFetch`. The query string is built by hand
 * rather than through the generated `listPublicApplications`: that hook models
 * the parameters but returns them through react-query's own cache, and this
 * hook merges pages into category buckets itself.
 *
 * ── The backend gap this hook was written against is closed ─────────────
 *
 * `PublicApplications` used to read exactly ONE query parameter, `category`,
 * and answered a hardcoded `ORDER BY a.id DESC LIMIT 50`, with no
 * `likes`/`is_liked` on any row. Search, sort, "My Liked" and every row past
 * the 50th were therefore impossible (issue #36 item 8). It now reads
 * `query`, `statuses`, `agents_type`, `sort_by`, `sort_order`, `limit`,
 * `offset` and `my_liked`, counts `total` over the FILTERED set, and puts
 * `tags`, `likes` and `is_liked` on every row
 * (`services/elitea-main/internal/api/v2/eliteacore/public_applications.go`).
 * So the parameters below are read, not merely sent: the search filters
 * server-side, Trending sorts by the real like count, My Liked filters by the
 * CALLER's likes, and "load more" asks for the next `offset` until the rows
 * in hand reach `total`. An out-of-allowlist `sort_by`/`sort_order`/
 * `agents_type`/`statuses` is a 400, not a silent fallback.
 *
 * @public Wave-2 unit A13 surface.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import {
  useGetAgentCategories,
} from '@/shared/api/generated/applications/applications';
import { eliteaFetch } from '@/shared/api/generated/mutator';
import { getConfig } from '@/shared/config';
import type { PublicApplicationList } from '@/shared/api/generated/model';
import type { AgentHubQueryOptions, AgentHubSortBy, AgentHubSortOrder, ApplicationData } from './types';

import {
  TRENDING_CATEGORY,
  MY_LIKED_CATEGORY,
  PAGE_SIZE,
  ALL_AGENTS_LIMIT,
} from './constants';
import { buildAllCategories, getCategoryForApplication } from './helpers';

/**
 * Public project id — per-deployment `VITE_PUBLIC_PROJECT_ID` runtime
 * config, read via `shared/config`'s `getConfig()` (adversarial-review fix,
 * cluster A13-agents-hub, finding 7: this was hardcoded to the literal
 * `'1'`, so any deployment whose real public project id differs queried the
 * wrong project's `agent_categories` and got zero categories back). Same
 * `getConfig()` convention every other `PUBLIC_PROJECT_ID` consumer in this
 * codebase uses — see e.g. `pages/agents/lib/isPublicAgentsProject.ts`.
 * `App.tsx` (unit R2) renders `MissingEnvPage` instead of mounting any route
 * when config status is `'missing'`, so by the time this hook runs config
 * is always `'ok'` — the `'1'` fallback below is unreachable in practice
 * and exists only so this stays a total function instead of throwing.
 */
function resolvePublicProjectId(): string {
  const config = getConfig();
  return config.status === 'ok' ? config.config.vite_public_project_id : '1';
}

/* ── Helpers ──────────────────────────────────────────────────────────── */

/**
 * Fetch one page of published applications. Every parameter passed here is
 * read by the handler; see this module's top-of-file doc comment.
 *
 * DEFECT, fixed here — two bugs in one function, each enough to empty the
 * whole hub:
 *  1. It called `fetch` with a bare `/elitea_core/...` path. Only the shared
 *     HTTP client adds the `/api/v2` base, so the request 404'd and the
 *     function threw. Every category rendered empty.
 *  2. It read `json.data?.rows`. The handler answers `{"rows":[],"total":0}`
 *     at the TOP level (`internal/api/v2/eliteacore/handler.go`), so `rows`
 *     was `undefined` and the `|| []` fallback emptied a good 200 response.
 * `eliteaFetch` resolves the base and returns orval's `{data,...}` envelope,
 * so the body is read from `envelope.data`.
 */
async function fetchAllApplications(params: Record<string, string>): Promise<{ rows: ApplicationData[]; total: number }> {
  const qs = new URLSearchParams(params);
  const envelope = await eliteaFetch<{ data: PublicApplicationList }>(
    `/elitea_core/public_applications/prompt_lib?${qs.toString()}`,
    { method: 'GET' },
  );
  const body = envelope.data;
  return { rows: body.rows ?? [], total: body.total ?? 0 };
}

/**
 * Normalise a rejection into an `Error`.
 *
 * `eliteaFetch` always rejects with an `EliteaApiError`, but a defensive
 * conversion keeps the state type honest for any other throw.
 */
function toLoadError(cause: unknown): Error {
  return cause instanceof Error ? cause : new Error(String(cause));
}

/* ── Hook ─────────────────────────────────────────────────────────────── */

export function useAgentHubData(_selectedTagNames: string[], options?: AgentHubQueryOptions) {
  const publicProjectId = resolvePublicProjectId();
  // Destructured to primitives on purpose: an options OBJECT default would be
  // a new reference every render, so every fetch callback below would be
  // rebuilt and the fetch effect would loop.
  const searchQuery = (options?.query ?? '').trim();
  const sortBy: AgentHubSortBy = options?.sortBy ?? 'created_at';
  const sortOrder: AgentHubSortOrder = options?.sortOrder ?? 'desc';
  const {
    data: categoriesData,
    isFetching: isFetchingCategories,
    error: categoriesError,
  } = useGetAgentCategories(publicProjectId, { query: { enabled: true } });

  const categoryNames = useMemo(() => {
    if (!categoriesData || categoriesData.status !== 200) return [];
    return (categoriesData.data.categories || []).map((c: { name: string }) => c.name);
  }, [categoriesData]);

  const [applicationsByTag, setApplicationsByTag] = useState<Record<string, ApplicationData[]>>({});
  const [totalCountsByTag, setTotalCountsByTag] = useState<Record<string, number>>({});
  const [loadingTags, setLoadingTags] = useState<Set<string>>(new Set());
  const [refreshingTags, setRefreshingTags] = useState<Set<string>>(new Set());
  /**
   * DEFECT, fixed here: the three fetches below used `try { … } finally { … }`
   * with NO `catch`, and the effect discarded each promise with `void`. Any
   * refusal became an unhandled rejection while the loading flag still
   * cleared, so a refused hub rendered as "No agents found". This state
   * carries the failure out so the page can tell the two apart.
   */
  const [loadError, setLoadError] = useState<Error | null>(null);

  /**
   * How many bulk rows are in hand and how many the server says match. The
   * pair is what makes "load more" honest: it stops asking when the rows held
   * reach `total`, instead of paging forever against a fixed row cap.
   */
  const [bulkLoaded, setBulkLoaded] = useState(0);
  const [bulkTotal, setBulkTotal] = useState(0);

  // ── Bulk fetch: one page, bucketed client-side by category ───────────
  const fetchAllAndCategorize = useCallback(async (offset = 0) => {
    setLoadingTags(prev => new Set(prev).add('bulk'));
    setLoadError(null);
    try {
      const result = await fetchAllApplications({
        limit: String(ALL_AGENTS_LIMIT),
        offset: String(offset),
        statuses: 'published',
        sort_by: sortBy,
        sort_order: sortOrder,
        ...(searchQuery === '' ? {} : { query: searchQuery }),
      });
      setBulkTotal(result.total);
      setBulkLoaded(offset + result.rows.length);
      setApplicationsByTag(prev => {
        // A fresh page 0 replaces the category buckets but KEEPS the two
        // special buckets: they are filled by their own requests, and
        // dropping them here made them blink out whenever the search text or
        // the sort changed.
        const next: Record<string, ApplicationData[]> = offset === 0 ? {} : { ...prev };
        if (offset === 0) {
          const trending = prev[TRENDING_CATEGORY];
          if (trending) next[TRENDING_CATEGORY] = trending;
          const myLiked = prev[MY_LIKED_CATEGORY];
          if (myLiked) next[MY_LIKED_CATEGORY] = myLiked;
        }
        result.rows.forEach((app: ApplicationData) => {
          const cat = getCategoryForApplication(app);
          const bucket = next[cat] ?? [];
          // A later page can repeat a row when the underlying set shifts
          // between requests. Appending it twice would duplicate a card.
          next[cat] = bucket.some(existing => existing.id === app.id) ? bucket : [...bucket, app];
        });
        setTotalCountsByTag(counts => {
          const updated: Record<string, number> = { ...counts };
          Object.entries(next).forEach(([category, items]) => {
            if (category === TRENDING_CATEGORY || category === MY_LIKED_CATEGORY) return;
            updated[category] = items.length;
          });
          return updated;
        });
        return next;
      });
    } catch (cause) {
      setLoadError(toLoadError(cause));
    } finally {
      setLoadingTags(prev => {
        const s = new Set(prev);
        s.delete('bulk');
        return s;
      });
    }
  }, [searchQuery, sortBy, sortOrder]);

  // ── Trending: sorted by likes ────────────────────────────────────────
  const fetchTrending = useCallback(async () => {
    setLoadingTags(prev => new Set(prev).add(TRENDING_CATEGORY));
    setLoadError(null);
    try {
      const result = await fetchAllApplications({
        limit: String(PAGE_SIZE),
        offset: '0',
        statuses: 'published',
        sort_by: 'likes',
        sort_order: 'desc',
        ...(searchQuery === '' ? {} : { query: searchQuery }),
      });
      setApplicationsByTag(prev => ({
        ...prev,
        [TRENDING_CATEGORY]: result.rows,
      }));
      setTotalCountsByTag(prev => ({
        ...prev,
        [TRENDING_CATEGORY]: result.total,
      }));
    } catch (cause) {
      setLoadError(toLoadError(cause));
    } finally {
      setLoadingTags(prev => {
        const s = new Set(prev);
        s.delete(TRENDING_CATEGORY);
        return s;
      });
    }
  }, [searchQuery]);

  // ── My Liked ─────────────────────────────────────────────────────────
  const fetchMyLiked = useCallback(async () => {
    setLoadingTags(prev => new Set(prev).add(MY_LIKED_CATEGORY));
    setLoadError(null);
    try {
      const result = await fetchAllApplications({
        limit: String(PAGE_SIZE),
        offset: '0',
        statuses: 'published',
        my_liked: 'true',
        ...(searchQuery === '' ? {} : { query: searchQuery }),
      });
      setApplicationsByTag(prev => ({
        ...prev,
        [MY_LIKED_CATEGORY]: result.rows,
      }));
      setTotalCountsByTag(prev => ({
        ...prev,
        [MY_LIKED_CATEGORY]: result.total,
      }));
    } catch (cause) {
      setLoadError(toLoadError(cause));
    } finally {
      setLoadingTags(prev => {
        const s = new Set(prev);
        s.delete(MY_LIKED_CATEGORY);
        return s;
      });
    }
  }, [searchQuery]);

  // ── Main fetch effect ────────────────────────────────────────────────
  // The three callbacks depend on the search text and the sort, so a change
  // to either re-runs this effect and the server answers the new query. The
  // page keeps NO client-side copy of the filter: a search that only matched
  // a row on page two would otherwise never be found.
  useEffect(() => {
    if (categoryNames.length === 0) return;
    void fetchAllAndCategorize(0);
    void fetchTrending();
    void fetchMyLiked();
  }, [categoryNames.length, fetchAllAndCategorize, fetchTrending, fetchMyLiked]);

  /**
   * Whether the server says there are rows the hub has not asked for yet.
   * `bulkTotal` counts the FILTERED set, so this is false as soon as the
   * search has been exhausted, not when a fixed cap is reached.
   */
  const hasMore = bulkLoaded < bulkTotal;

  /** Ask for the next page and merge it into the category buckets. */
  const loadMore = useCallback(async () => {
    if (!hasMore || loadingTags.has('bulk')) return;
    await fetchAllAndCategorize(bulkLoaded);
  }, [hasMore, loadingTags, bulkLoaded, fetchAllAndCategorize]);

  // ── Derived data ─────────────────────────────────────────────────────
  const allCategories = useMemo(
    () => buildAllCategories(categoryNames),
    [categoryNames],
  );

  const isFetching = useMemo(
    () => loadingTags.size > 0 || isFetchingCategories,
    [loadingTags.size, isFetchingCategories],
  );

  /**
   * The categories query fails on its own path. `categoryNames` is then
   * empty, the fetch effect above returns early, and `loadError` stays
   * `null`. A refused `agent_categories` request therefore also produced a
   * silent empty hub. Both failures are folded into one value the page
   * renders.
   */
  const error = useMemo<Error | null>(
    () => loadError ?? (categoriesError instanceof Error ? categoriesError : null),
    [loadError, categoriesError],
  );

  // ── State updates ────────────────────────────────────────────────────
  const updateApplicationInState = useCallback(
    (applicationId: string, updateFn: (app: ApplicationData) => ApplicationData) => {
      setApplicationsByTag(prev => {
        const updated: Record<string, ApplicationData[]> = {};
        Object.keys(prev).forEach(cat => {
          const list = prev[cat];
          if (!list) return;
          updated[cat] = list.map(app =>
            app.id === applicationId ? updateFn(app) : app,
          );
        });
        return updated;
      });
    },
    [],
  );

  const addToMyLiked = useCallback((application: ApplicationData) => {
    setApplicationsByTag(prev => ({
      ...prev,
      [MY_LIKED_CATEGORY]: [...(prev[MY_LIKED_CATEGORY] || []), application],
    }));
  }, []);

  const removeFromMyLiked = useCallback((applicationId: string) => {
    setApplicationsByTag(prev => ({
      ...prev,
      [MY_LIKED_CATEGORY]: (prev[MY_LIKED_CATEGORY] || []).filter(a => a.id !== applicationId),
    }));
  }, []);

  /**
   * Each fetch below handles its own rejection, so this promise always
   * settles. That matters here. `onRefresh` runs from a click handler. A
   * rejection there would escape as an unhandled rejection instead of an
   * error the section can show.
   */
  const onRefresh = useCallback(
    async (category: string) => {
      setRefreshingTags(prev => new Set(prev).add(category));
      try {
        if (category === TRENDING_CATEGORY) {
          await fetchTrending();
        } else if (category === MY_LIKED_CATEGORY) {
          await fetchMyLiked();
        } else {
          await fetchAllAndCategorize(0);
        }
      } finally {
        setRefreshingTags(prev => {
          const s = new Set(prev);
          s.delete(category);
          return s;
        });
      }
    },
    [fetchTrending, fetchMyLiked, fetchAllAndCategorize],
  );

  // ── Filtered data (by selected tags) ─────────────────────────────────
  const filteredByTag = useMemo(() => {
    if (_selectedTagNames.length === 0) return applicationsByTag;
    const filtered: Record<string, ApplicationData[]> = {};
    _selectedTagNames.forEach((tag: string) => {
      if (applicationsByTag[tag]) filtered[tag] = applicationsByTag[tag];
    });
    return filtered;
  }, [applicationsByTag, _selectedTagNames]);

  return {
    categoryNames,
    allCategories,
    applicationsByTag: filteredByTag,
    totalCountsByTag,
    loadingTags,
    refreshingTags,
    isFetching,
    error,
    hasMore,
    loadMore,
    totalCount: bulkTotal,
    updateApplicationInState,
    addToMyLiked,
    removeFromMyLiked,
    onRefresh,
  };
}
