/**
 * The "Elitea" part of the desktop sidebar: every web feature, reached from
 * a compact list under the folders. The rows are the web sidebar's own nav
 * model (`navSections` + its permission filter), so a feature the web app
 * hides from someone is hidden here too; the Catalog and Help Center rows
 * the web sidebar keeps in its footer follow them.
 */
import { navSections, visibleNavSections, type NavItemValue } from '@/widgets/sidebar';

export type EliteaItemValue = Exclude<NavItemValue, 'workspaces'> | 'catalog' | 'help';

export interface EliteaItem {
  value: EliteaItemValue;
  label: string;
  url: string;
}

export function eliteaItems(permissions: ReadonlySet<string>, isSelectedProjectPublic: boolean): EliteaItem[] {
  const nav = visibleNavSections(navSections(), permissions, { isSelectedProjectPublic })
    .flatMap((section) => section.items)
    .filter((item): item is typeof item & { value: Exclude<NavItemValue, 'workspaces'> } => item.value !== 'workspaces')
    .map((item) => ({ value: item.value, label: item.label, url: item.url }));
  return [
    ...nav,
    { value: 'catalog', label: 'Catalog', url: '/elitea-catalog' },
    { value: 'help', label: 'Help Center', url: '/help-center' },
  ];
}

/** Which row a pathname belongs to (`undefined` on the folders and on settings). */
export function selectedEliteaItem(pathname: string, items: readonly EliteaItem[]): EliteaItemValue | undefined {
  return items.find((item) => pathname === item.url || pathname.startsWith(`${item.url}/`))?.value;
}
