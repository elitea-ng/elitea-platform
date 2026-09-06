/**
 * Ported from `apps/elitea-ui/src/[fsd]/shared/lib/hooks/
 * useGroupedCategories.hooks.js` — the search + category-chip + grouping
 * state machine behind every "choose a type" catalogue screen (toolkit type,
 * MCP type, credential type).
 *
 * Both ported callers (`useToolkitSearch.js`, `useCredentialSearch.js`) drove
 * `Category.GroupedCategory` with the object this returns; this app's
 * `ToolkitTypeSelector`/`CredentialTypeSelector` previously had NO grouping
 * at all — one flat, chip-less grid — which is the visual gap this restores.
 *
 * DISCLOSED DEVIATIONS from the baseline hook:
 *  - `options.isSearchDisabled`/`isSortDisabled` and the
 *    `categoryOrderByServer` per-category ordering branch are dropped: no
 *    caller in this app passes either (both baseline call sites use the
 *    3-argument form), and `categoryOrderByServer` is not a field any
 *    ported item type carries.
 *  - The baseline mutates `filtered` in place with `.sort()` on a memoized
 *    array (it sorts `allItems`' own filtered view). This port sorts a copy —
 *    same order, no shared-array mutation.
 */
import { useCallback, useMemo, useState, type ChangeEvent, type ReactNode } from 'react';

/**
 * The structural shape this hook groups. Deliberately NOT imported from
 * `shared/ui/CategoryItemCard` — `shared/lib` must not depend on
 * `shared/ui` — but assignable to that component's `CategoryItem`.
 */
export interface GroupableItem {
  readonly key: string;
  readonly label: string;
  readonly icon?: ReactNode;
  /** Free-text bucket searched alongside the label (baseline: `item.section`). */
  readonly section?: string;
  /** Optional sub-heading inside a category (`CategorySection`'s `enableSubGroups`). */
  readonly group?: string;
}

/** One grouped entry: the input item plus the resolved click handler and categories. */
interface GroupedCategoryItem extends GroupableItem {
  readonly categories: readonly string[];
  readonly onClick: () => void;
}

export interface UseGroupedCategoriesResult {
  readonly allCategories: readonly string[];
  readonly groupedItems: Readonly<Record<string, readonly GroupedCategoryItem[]>>;
  readonly selectedCategories: readonly string[];
  readonly searchQuery: string;
  readonly onSearchChange: (event: ChangeEvent<HTMLInputElement>) => void;
  readonly onSelectCategory: (category: string) => void;
}

function toCategoryList(raw: string | readonly string[] | undefined): readonly string[] {
  if (typeof raw === 'string') return raw === '' ? ['Other'] : [raw];
  return raw !== undefined && raw.length > 0 ? raw : ['Other'];
}

function matchesQuery(item: GroupedCategoryItem, needle: string): boolean {
  return (
    item.label.toLowerCase().includes(needle) ||
    item.key.toLowerCase().includes(needle) ||
    (item.section?.toLowerCase().includes(needle) ?? false) ||
    item.categories.some((category) => category.toLowerCase().includes(needle))
  );
}

/**
 * @param items       the catalogue entries, already labelled.
 * @param categoryOf  resolves an item's category (or categories — an item may
 *                    appear under several).
 * @param onSelect    invoked with the ORIGINAL item when its tile is clicked.
 * @param specialGroup a category pinned first in the chip row even when no
 *                    item carries it (baseline: MCP's empty `Local` bucket,
 *                    which is what makes the "Still no local MCP available"
 *                    placeholder reachable).
 */
export function useGroupedCategories<T extends GroupableItem>(
  items: readonly T[],
  categoryOf: (item: T) => string | readonly string[] | undefined,
  onSelect: (item: T) => void,
  specialGroup?: string,
): UseGroupedCategoriesResult {
  const [selectedCategories, setSelectedCategories] = useState<readonly string[]>([]);
  const [searchQuery, setSearchQuery] = useState('');

  const allItems = useMemo<readonly GroupedCategoryItem[]>(
    () =>
      items.map((item) => ({
        ...item,
        categories: toCategoryList(categoryOf(item)),
        onClick: () => {
          onSelect(item);
        },
      })),
    [items, categoryOf, onSelect],
  );

  const allCategories = useMemo<readonly string[]>(() => {
    const set = new Set<string>();
    for (const item of allItems) for (const category of item.categories) set.add(category);
    const sorted = [...set].sort((a, b) => a.localeCompare(b));
    return specialGroup !== undefined && !set.has(specialGroup) ? [specialGroup, ...sorted] : sorted;
  }, [allItems, specialGroup]);

  const filteredItems = useMemo<readonly GroupedCategoryItem[]>(() => {
    let filtered = allItems;
    if (selectedCategories.length > 0) {
      filtered = filtered.filter((item) => item.categories.some((category) => selectedCategories.includes(category)));
    }
    const needle = searchQuery.trim().toLowerCase();
    if (needle !== '') filtered = filtered.filter((item) => matchesQuery(item, needle));
    return [...filtered].sort((a, b) => a.label.localeCompare(b.label));
  }, [allItems, selectedCategories, searchQuery]);

  const groupedItems = useMemo<Readonly<Record<string, readonly GroupedCategoryItem[]>>>(() => {
    const groups: Record<string, GroupedCategoryItem[]> = {};
    for (const item of filteredItems) {
      for (const category of item.categories) {
        if (selectedCategories.length > 0 && !selectedCategories.includes(category)) continue;
        (groups[category] ??= []).push(item);
      }
    }
    return groups;
  }, [filteredItems, selectedCategories]);

  const onSelectCategory = useCallback((category: string) => {
    setSelectedCategories((prev) => (prev.includes(category) ? prev.filter((entry) => entry !== category) : [...prev, category]));
  }, []);

  const onSearchChange = useCallback((event: ChangeEvent<HTMLInputElement>) => {
    setSearchQuery(event.target.value);
  }, []);

  return { allCategories, groupedItems, selectedCategories, searchQuery, onSearchChange, onSelectCategory };
}
