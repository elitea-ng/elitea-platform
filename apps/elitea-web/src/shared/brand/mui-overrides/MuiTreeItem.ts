import type { EliteaOverrideTheme } from '../theme-types';
import { typeScale } from '../typography';

/**
 * `MuiTreeItem` (R-T12). Ported from `apps/elitea-ui/src/components/TreeItem.jsx`'s
 * `eliteaTreeItemStyle`. The baseline's `isActive` prop toggled the
 * first-level content background between `background.select.hover` and
 * `transparent` — folded into the `.Mui-selected` state class here instead,
 * since `isActive` was the caller manually re-deriving MUI's own selection
 * state.
 *
 * NOT WIRED into `muiOverrides()` / `index.ts`'s export (deliberately — see
 * that file's comment): the `Mui*` key this file styles belongs to
 * `@mui/x-tree-view`'s `TreeItem`, and that package is intentionally not a
 * dependency of this app (spec §2.2 / P1: "1 import; the file tree is
 * hand-rolled"). Registering a `styleOverrides`/`variants` entry for a
 * component that is never rendered would be dead theme config, and there is
 * no way to prove it correct via the §4.6 check 7 render sweep without
 * installing the very package the spec says to avoid. The file is kept,
 * complete and ready, so the day a hand-rolled tree wants these exact rules
 * (as literal class-name-keyed styles, or because `@mui/x-tree-view` gets
 * reintroduced) porting them is a one-line `import` + object-literal add in
 * `index.ts`, not a re-derivation from the baseline.
 *
 * Typed loosely (not against `Components<Theme>['MuiTreeItem']`, which does
 * not exist without the `@mui/x-tree-view/themeAugmentation` import this
 * file deliberately does not add) — this is documentation-shaped config,
 * not a value any consumer's types depend on.
 */
export const MuiTreeItem = {
  variants: [
    {
      props: { variant: 'elitea' },
      style: ({ theme }: { theme: EliteaOverrideTheme }) => {
        const { palette } = theme.vars;
        return {
          '& .MuiTreeItem-label': {
            ...typeScale(theme.typography.bodyMedium),
            color: palette.text.secondary,
          },
          '& .MuiTreeItem-content:hover': {
            background: palette.background.userInputBackground,
          },
          '& .MuiTreeItem-content.Mui-selected': {
            backgroundColor: palette.background.select.selected.default,
          },
          '& .Mui-selected > .MuiTreeItem-label:hover': {
            background: 'transparent',
          },
          '& .Mui-selected:hover': {
            background: palette.background.userInputBackground,
          },
          '& .Mui-selected': {
            backgroundColor: 'transparent',
            '& .MuiTreeItem-checkbox svg': {
              fill: palette.icon.fill.secondary,
            },
          },
          '& .MuiTreeItem-checkbox svg': {
            fill: palette.icon.fill.secondary,
          },
          '& .Mui-focused': {
            background: 'transparent',
          },
        };
      },
    },
  ],
};
