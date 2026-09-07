import type { EliteaComponents } from '../theme-types';

import { MuiAccordion } from './MuiAccordion';
import { MuiAlert } from './MuiAlert';
import { MuiAppBar } from './MuiAppBar';
import { MuiAutocomplete } from './MuiAutocomplete';
import { MuiAvatar } from './MuiAvatar';
import { MuiBadge } from './MuiBadge';
import { MuiButton } from './MuiButton';
import { MuiCheckbox } from './MuiCheckbox';
import { MuiChip } from './MuiChip';
import { MuiCssBaseline } from './MuiCssBaseline';
import { MuiDataGrid } from './MuiDataGrid';
import { MuiDialog } from './MuiDialog';
import { MuiDrawer } from './MuiDrawer';
import { MuiFormControl } from './MuiFormControl';
import { MuiFormControlLabel } from './MuiFormControlLabel';
import { MuiFormHelperText } from './MuiFormHelperText';
import { MuiIconButton } from './MuiIconButton';
import { MuiInput } from './MuiInput';
import { MuiList } from './MuiList';
import { MuiMenu } from './MuiMenu';
import { MuiMenuItem } from './MuiMenuItem';
import { MuiOutlinedInput } from './MuiOutlinedInput';
import { MuiPaper } from './MuiPaper';
import { MuiRadio } from './MuiRadio';
import { MuiSelect } from './MuiSelect';
import { MuiSwitch } from './MuiSwitch';
import { MuiTab } from './MuiTab';
import { MuiTablePagination } from './MuiTablePagination';
import { MuiTabs } from './MuiTabs';
import { MuiTextField } from './MuiTextField';
import { MuiToggleButton } from './MuiToggleButton';
import { MuiTooltip } from './MuiTooltip';
import { MuiTypography } from './MuiTypography';

// `MuiTreeItem` is intentionally NOT imported/wired — see MuiTreeItem.ts's
// header comment. Its target library (`@mui/x-tree-view`) is not a
// dependency of this app (spec §2.2/P1); the file is written and ready, not
// wired, so it costs nothing to the §4.6 check 7 render sweep (which can
// only prove a key correct by rendering the real component it styles).

/**
 * The R-T12 override package: MUI `styleOverrides` exist ONLY under this
 * directory, one file per `Mui*` key, and every one of them reads
 * `theme.vars.*` — no raw colours, no scheme branches, no `!important`
 * beyond the two documented, MUI-internal-CSS-custom-property exceptions in
 * `MuiDataGrid.ts`.
 *
 * Composition is a plain object literal so the set of wired keys is
 * greppable and the file stays a table of contents rather than logic.
 *
 * Unit T1 wired `MuiButton`/`MuiChip`. Unit S1 wires 27 of the remaining 28
 * keys named in OWNERSHIP.md (all but `MuiTreeItem`, see above), plus
 * `MuiTypography` (R-C2 — not one of the baseline's 30 keys; see that
 * file's own doc comment). The platform-parity wave adds `MuiAccordion` —
 * not a baseline key either, see `MuiAccordion.ts`'s own doc comment.
 *
 * The trailing `as EliteaComponents` is one reviewed, necessary cast:
 * `Components<Theme>`'s entries are `MuiX?: ComponentsOverrides['MuiX']`
 * (optional, not `| undefined`), but each per-file export is typed via
 * `EliteaComponents['MuiX']` — an indexed access on an optional member,
 * which TypeScript resolves as `X | undefined`. Under
 * `exactOptionalPropertyTypes` those are NOT the same type (a present
 * property valued `undefined` differs from an absent property), so the
 * object literal below — structurally correct at runtime, every value is a
 * real object — fails the direct contextual check. Retyping all 30
 * per-file exports through `NonNullable<...>` would be the alternative;
 * one assertion at the single composition point is less churn for the same
 * guarantee.
 */
export function muiOverrides(): EliteaComponents {
  return {
    MuiAccordion,
    MuiAlert,
    MuiAppBar,
    MuiAutocomplete,
    MuiAvatar,
    MuiBadge,
    MuiButton,
    MuiCheckbox,
    MuiChip,
    MuiCssBaseline,
    MuiDataGrid,
    MuiDialog,
    MuiDrawer,
    MuiFormControl,
    MuiFormControlLabel,
    MuiFormHelperText,
    MuiIconButton,
    MuiInput,
    MuiList,
    MuiMenu,
    MuiMenuItem,
    MuiOutlinedInput,
    MuiPaper,
    MuiRadio,
    MuiSelect,
    MuiSwitch,
    MuiTab,
    MuiTablePagination,
    MuiTabs,
    MuiTextField,
    MuiToggleButton,
    MuiTooltip,
    MuiTypography,
  } as EliteaComponents;
}
