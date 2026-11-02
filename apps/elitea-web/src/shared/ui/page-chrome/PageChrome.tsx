/**
 * The page title belongs to the shell, not to the page.
 *
 * Both shells draw one title row: the desktop frame's `PageTitle` and the
 * web shell's `ShellTopBar`. Each shows a breadcrumb whose first crumb is the
 * section the pathname is in (Agents, Toolkits, Settings…) and whose last crumb
 * is the page's own heading. A page that is more than its section — a create
 * form, an entity's editor, the archived chats — names itself here with
 * `usePageCrumbs([{ label }])` instead of drawing an `<h1>` and a back arrow of
 * its own: the section crumb links back to the list, so the page needs
 * neither.
 *
 * The shell mounts `PageChromeProvider` and reads `useShellCrumbs()`. Outside
 * a provider (a unit test mounting a page alone, the desktop Settings dialog,
 * which sits over the frame rather than in its main column) there is no title
 * row, so `PageCrumbFallback` draws the heading in the page instead.
 */
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';

import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

/** One crumb after the section: its label, and the route it links to (none for the last one). */
export interface PageCrumb {
  readonly label: string;
  readonly to?: string;
}

interface PageChrome {
  /** The crumbs of the newest page that registered some; empty when none did. */
  readonly crumbs: readonly PageCrumb[];
  readonly register: (crumbs: readonly PageCrumb[]) => () => void;
}

const PageChromeContext = createContext<PageChrome | null>(null);

let nextRegistration = 0;

export function PageChromeProvider({ children }: { readonly children: ReactNode }): ReactNode {
  const [registrations, setRegistrations] = useState<readonly { readonly key: number; readonly crumbs: readonly PageCrumb[] }[]>([]);

  const register = useCallback((crumbs: readonly PageCrumb[]) => {
    nextRegistration += 1;
    const key = nextRegistration;
    setRegistrations((current) => [...current, { key, crumbs }]);
    return () => setRegistrations((current) => current.filter((entry) => entry.key !== key));
  }, []);

  const value = useMemo<PageChrome>(() => ({ crumbs: registrations.at(-1)?.crumbs ?? [], register }), [registrations, register]);
  return <PageChromeContext.Provider value={value}>{children}</PageChromeContext.Provider>;
}

/**
 * The page names itself in the shell's title row: `crumbs` follow the section
 * crumb, the last one is the page's heading. Withdrawn when the page unmounts.
 */
export function usePageCrumbs(crumbs: readonly PageCrumb[]): void {
  const register = useContext(PageChromeContext)?.register;
  // A fresh array every render must not re-register: the crumbs are compared by what they say.
  const signature = JSON.stringify(crumbs);
  useEffect(() => {
    if (register === undefined) return undefined;
    return register(JSON.parse(signature) as PageCrumb[]);
  }, [register, signature]);
}

/** The shell's side: what the page on screen registered (empty: the section's own page). */
export function useShellCrumbs(): readonly PageCrumb[] {
  return useContext(PageChromeContext)?.crumbs ?? [];
}

/** `true` inside a shell that draws the page title. */
function useHasPageChrome(): boolean {
  return useContext(PageChromeContext) !== null;
}

/** The page's heading where no shell draws it (see the module doc); nothing inside a shell. */
export function PageCrumbFallback({ title, sx }: { readonly title: string; readonly sx?: SxProps<Theme> }): ReactNode {
  if (useHasPageChrome()) return null;
  return (
    <Typography
      variant="headingMedium"
      component="h1"
      color="text.secondary"
      {...(sx === undefined ? {} : { sx })}
    >
      {title}
    </Typography>
  );
}
