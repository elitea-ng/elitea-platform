/**
 * The "Elitea" part of the desktop sidebar: every web feature, reached from
 * a compact list under the folders. The rows are the web sidebar's own nav
 * model (`navSections` + its permission filter), so a feature the web app
 * hides from someone is hidden here too; the Catalog and Help Center rows
 * the web sidebar keeps in its footer follow them.
 *
 * The sidebar lists all but {@link ACCOUNT_MENU_ITEMS}, which the account
 * menu at its foot carries, and keeps the things a person builds with in one
 * collapsible "Workshop" group (`NavRows`, `widgets/chat-rail`), so the list
 * shows the everyday places first; the command palette and the page title
 * still know every item. An item that can create carries its create page
 * (`create`), for the row's "+".
 */
import { t } from '@/shared/i18n';
import { navSections, visibleNavSections, type NavItemValue } from '@/widgets/sidebar';

export type EliteaItemValue = Exclude<NavItemValue, 'workspaces'> | 'catalog' | 'help';

export interface EliteaItem {
  value: EliteaItemValue;
  label: string;
  url: string;
  create?: { readonly url: string; readonly label: string };
}

export function eliteaItems(permissions: ReadonlySet<string>, isSelectedProjectPublic: boolean): EliteaItem[] {
  const nav = visibleNavSections(navSections(), permissions, { isSelectedProjectPublic })
    .flatMap((section) => section.items)
    .filter((item): item is typeof item & { value: Exclude<NavItemValue, 'workspaces'> } => item.value !== 'workspaces')
    .map((item) => ({ value: item.value, label: item.label, url: item.url, ...(item.create !== undefined ? { create: item.create } : {}) }));
  return [
    ...nav,
    { value: 'catalog', label: t('desktop.shell.catalog', 'Catalog'), url: '/elitea-catalog' },
    { value: 'help', label: t('desktop.shell.helpCenter', 'Help Center'), url: '/help-center' },
  ];
}

/** The items the account menu carries instead of the sidebar's Elitea list. */
export const ACCOUNT_MENU_ITEMS: ReadonlySet<EliteaItemValue> = new Set(['credentials', 'catalog', 'help']);

/** Which row a pathname belongs to (`undefined` on the folders and on settings). */
export function selectedEliteaItem(pathname: string, items: readonly EliteaItem[]): EliteaItemValue | undefined {
  return items.find((item) => pathname === item.url || pathname.startsWith(`${item.url}/`))?.value;
}
