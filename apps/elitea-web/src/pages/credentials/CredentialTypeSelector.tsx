/**
 * pages/credentials/CredentialTypeSelector.tsx — the "choose a credential
 * type to create" grid, shown by `CredentialForm` before a type is picked.
 * Ported from `apps/elitea-ui/src/pages/Credentials/CredentialTypeSelector.jsx`.
 *
 * CORRECTED (visual-parity pass). Two of this file's three disclosed
 * simplifications are closed, because both were VISIBLE on screen and both had
 * a real implementation available:
 *  - The baseline renders `Category.GroupedCategory`, i.e. the same
 *    centred title + rounded search field + category-chip row + per-category
 *    uppercase section chrome the toolkit/MCP pickers use. This port rendered
 *    a bare `<div>` with a `SimpleSearchBar` above an ungrouped list: no
 *    title, no chips, nothing centred. It now composes `CategoryFilter` around
 *    `GroupedCategory`, exactly as `features/toolkits`' `ToolkitTypeSelector`
 *    does, with `shared/lib/hooks/useGroupedCategories` (the ported baseline
 *    hook) owning search + chip state instead of the local `useState`.
 *  - The per-type icon (baseline `getToolIconByType`) is restored via
 *    `shared/ui/ToolkitTypeIcon`; every tile used to render with no leading
 *    glyph.
 *
 * STILL DISCLOSED: this screen has no sub-header of its own ("New Credential"
 * / "New Configuration" with the route's own back affordance). That bar
 * belongs to the credentials route/page shell, which also wraps the credential
 * FORM — out of this change's fence, and noted rather than silently added.
 */
import { useCallback, useMemo, type ReactNode } from 'react';

import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { useGroupedCategories } from '@/shared/lib/hooks/useGroupedCategories';
import { CategoryFilter } from '@/shared/ui/CategoryFilter';
import { CategorySection } from '@/shared/ui/CategorySection';
import { GroupedCategory } from '@/shared/ui/GroupedCategory';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';
import { ToolkitTypeIcon } from '@/shared/ui/ToolkitTypeIcon';
import type { CategoryItem } from '@/shared/ui/CategoryItemCard';

import type { ConfigurationTypeDescriptor } from '@/features/credentials';

export interface CredentialTypeSelectorProps {
  readonly configurationsData: readonly ConfigurationTypeDescriptor[] | undefined;
  readonly isFetching: boolean;
  readonly onSelectType: (type: string) => void;
}

/**
 * `config_schema` is REQUIRED by `ConfigurationTypeDescriptor`, but the wire
 * is not the type system: a catalogue entry that omits it used to throw
 * "Cannot read properties of undefined (reading 'metadata')" out of these two
 * readers, which the route has no boundary below, so the whole
 * /settings/create-configuration page rendered "Something went wrong."
 * (#131). One malformed row must not take out the picker — an entry with no
 * schema degrades to its raw `type` under "Other", and the rest of the
 * catalogue still renders.
 */
function displayLabel(item: ConfigurationTypeDescriptor): string {
  return item.config_schema?.metadata?.label ?? item.config_schema?.title ?? item.type;
}

function categoryOf(item: ConfigurationTypeDescriptor): string {
  return item.config_schema?.properties?.['data']?.metadata?.categories?.[0] ?? 'Other';
}

interface CredentialTypeMenuItem {
  readonly key: string;
  readonly label: string;
  readonly category: string;
  readonly icon: ReactNode;
}

function itemCategory(item: CredentialTypeMenuItem): string {
  return item.category;
}

/** Same full-width centred column `ToolkitTypeSelector` needs — see that file. */
const groupedItemsSx: SxProps<Theme> = (theme: Theme) => ({
  width: '100%',
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  gap: theme.spacing(3),
});

export function CredentialTypeSelector({ configurationsData, isFetching, onSelectType }: CredentialTypeSelectorProps): ReactNode {
  const items = useMemo<readonly CredentialTypeMenuItem[]>(
    () =>
      (configurationsData ?? [])
        .filter((item) => item.config_schema?.metadata?.hidden !== true)
        .map((item) => ({ key: item.type, label: displayLabel(item), category: categoryOf(item), icon: <ToolkitTypeIcon type={item.type} /> })),
    [configurationsData],
  );

  const handleSelect = useCallback((item: CredentialTypeMenuItem) => { onSelectType(item.key); }, [onSelectType]);

  const { allCategories, groupedItems, selectedCategories, searchQuery, onSearchChange, onSelectCategory } = useGroupedCategories<CredentialTypeMenuItem>(
    items,
    itemCategory,
    handleSelect,
  );

  const renderCategory = useCallback(
    (category: string, categoryItems: readonly CategoryItem[]): ReactNode => (
      <CategorySection
        category={category}
        items={categoryItems}
      />
    ),
    [],
  );

  return (
    <CategoryFilter
      title={t('credentials.typeSelector.title', 'Choose the credentials type')}
      searchPlaceholder={t('credentials.typeSelector.search', 'Search credentials')}
      searchQuery={searchQuery}
      onSearchChange={onSearchChange}
      allCategories={[...allCategories]}
      selectedCategories={[...selectedCategories]}
      onSelectCategory={onSelectCategory}
    >
      <GroupedCategory
        isLoading={isFetching}
        allCategories={allCategories}
        selectedCategories={selectedCategories}
        groupedItems={groupedItems}
        renderCategory={renderCategory}
        noResultsSlot={
          <NoResultsMessage
            title={t('credentials.typeSelector.noResultsTitle', 'No credentials found')}
            description={t('credentials.typeSelector.noResultsDescription', 'Try adjusting your search terms or category filters')}
          />
        }
        sx={groupedItemsSx}
      />
    </CategoryFilter>
  );
}
