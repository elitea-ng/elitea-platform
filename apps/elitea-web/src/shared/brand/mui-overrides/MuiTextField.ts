import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/**
 * `MuiTextField` (R-T12). Ported from `input/textFieldVariants.js`'s
 * `eliteaTextFieldVariants` (baseline `'standard'` variant). Simplified from
 * the baseline's `underline`-colour-by-state table to the token equivalents;
 * the `Mui-error`/`Mui-disabled`/`Mui-focused` state selectors are preserved
 * verbatim since they are MUI's own state classes, not brand-fork additions.
 *
 * [S1-E follow-up] The `.MuiFormHelperText-root.Mui-error` rule below used
 * `icon.fill.error` for the message TEXT colour. At the time, it was
 * `#D71616` in both colour schemes (`shared/brand/tokens/default.pack.json`),
 * the same non-text-tuned red flagged in `MuiFormHelperText.ts`'s own
 * follow-up comment (3.55:1 against a dark surface, short of WCAG AA's
 * 4.5:1 for text) — a concurrent brand-pack fix has since raised dark-scheme
 * `icon.fill.error` itself to `#ED4F4F` (5.16:1, passing on its own), but
 * `text.warningText` is kept below regardless, for the same
 * established-convention reason `MuiFormHelperText.ts` gives. This selector
 * (`.MuiTextField-root .MuiFormHelperText-root.Mui-error`, 3 classes) wins
 * the cascade over `MuiFormHelperText.ts`'s own `&.Mui-error` override (2
 * classes) for every `MuiTextField`'s helper text specifically — confirmed
 * by rendering a real `variant="standard"` `error` `TextField` and reading
 * `getComputedStyle(...).color` before and after this change, since the two
 * rules' equal-looking class selectors made this a real "which one wins"
 * question, not a guess. Caught by this unit's own Storybook a11y gate
 * (`CommonNumberField`'s `Invalid` story). `icon.fill.error` is left alone
 * on the border-colour rule two lines above — border/UI-component contrast
 * has a looser 3:1 WCAG threshold that value clears either way; only the
 * TEXT use needed the swap to `text.warningText` (the token already
 * established elsewhere in this codebase — `CharacterCounter`/
 * `BannerMessage` — for exactly this failure mode).
 *
 * [T1 follow-up] `.MuiFormLabel-root.Mui-error` had no rule here at all, so
 * MUI's own built-in `FormLabel` default (`&.Mui-error { color:
 * theme.vars.palette.error.main }`) rendered the field label directly —
 * `#D71616` in the dark scheme, 3.55:1 against `background.default`, the
 * exact violation `SecretField`'s `ErrorState` story was waived for
 * (`shared/ui/SecretField/SecretField.stories.tsx`). `error.main` cannot be
 * relightened to fix this without dropping `error.contrastText`'s own
 * 4.5:1 on `MuiAlert`'s filled surface below AA (exhaustive search over the
 * hue-0 ramp, `parity/brand-hue-map.md` §10: best achievable balance is
 * 4.31:1 on both sides, short of 4.5 on both) — it is a fill role paired
 * with `contrastText`, not a text role. Same fix as the `FormHelperText`
 * rule above: point the label at `text.warningText` instead.
 */
export const MuiTextField: EliteaComponents['MuiTextField'] = {
  variants: [
    {
      props: { variant: 'standard' },
      style: ({ theme }) => ({
        padding: `${theme.spacing(1)} 0 0 0`,
        '& .MuiFormLabel-root': {
          color: theme.vars.palette.text.primary,
        },
        '& .MuiFormLabel-root.Mui-disabled': {
          color: theme.vars.palette.text.button.disabled,
        },
        '& .MuiFormLabel-root.Mui-error': {
          color: theme.vars.palette.text.warningText,
        },
        '& input, & textarea': {
          // Input text is `bodyMedium` — the same role in every field, app
          // and admin (typography spec §2). Without the typography spread the
          // input kept MUI's stock 1rem body font.
          ...typeScale(theme.typography.bodyMedium),
          boxSizing: 'border-box',
          marginBottom: theme.spacing(1),
          color: theme.vars.palette.text.secondary,
        },
        '& input.Mui-disabled, & textarea.Mui-disabled': {
          color: theme.vars.palette.text.default,
          WebkitTextFillColor: theme.vars.palette.text.default,
        },
        '& input::placeholder, & textarea::placeholder': {
          color: theme.vars.palette.text.participant.default,
          opacity: 1,
        },
        '& :not(.Mui-error).MuiInput-underline:before': {
          borderBottomColor: theme.vars.palette.border.lines,
        },
        '& :not(.Mui-error, .Mui-disabled).MuiInput-underline:hover:before': {
          borderBottomColor: theme.vars.palette.border.hover,
        },
        '& :not(.Mui-error) label.Mui-focused': {
          color: theme.vars.palette.primary.main,
        },
        '& :not(.Mui-error).MuiInput-underline.Mui-focused:after': {
          borderBottomColor: theme.vars.palette.primary.main,
        },
        '& .Mui-error.MuiInput-underline:before, & .Mui-error.MuiInput-underline:after': {
          borderBottomColor: theme.vars.palette.icon.fill.error,
        },
        '& .MuiFormHelperText-root.Mui-error': {
          color: theme.vars.palette.text.warningText,
          paddingLeft: theme.spacing(1.5),
        },
        '&:has(.MuiInputBase-root.Mui-disabled), & .MuiInputBase-root.Mui-disabled': {
          cursor: 'not-allowed',
        },
        // Baseline `textFieldVariants.js:130-209` — number fields keep the
        // native spinners without these, which the baseline hides.
        '& input': { height: '1.5rem' },
        '& input[type=number]': { MozAppearance: 'textfield' },
        '& input[type=number]::-webkit-outer-spin-button, & input[type=number]::-webkit-inner-spin-button': {
          WebkitAppearance: 'none',
          margin: 0,
        },
        '& textarea::-webkit-scrollbar': { display: 'none' },
        '& .MuiInput-underline': { padding: `${theme.spacing(0.5)} ${theme.spacing(1.5)} 0` },
      }),
    },
    // `outlined` — NOT ported, and that is now a measured decision rather
    // than an omission.
    //
    // The baseline has an `outlined` variant
    // (`textFieldVariants.js:212-257`) that hides the notch legend and lifts
    // the outline to `top: 0`, so its label sits ON the border rather than in
    // a gap. Porting it verbatim BROKE every outlined field in this app: the
    // admin Configuration page's "Company Name for Policy Message" and
    // "Authorization Message Template" collapsed, with label and helper text
    // overlapping a flattened box (caught by regenerating
    // `admin-configuration-visual` and diffing it against the committed
    // baseline, not by reading the diff of the two stylesheets).
    //
    // The reason is that the baseline's rule is only coherent alongside the
    // rest of its outlined stack — its own label positioning, and `MuiFormLabel`
    // behaviour this app does not reproduce. This app already has a working
    // outlined treatment via `MuiOutlinedInput.tsx`, which the before-shot
    // shows rendering correctly. Adding a second, partial one on top is what
    // did the damage.
    //
    // If outlined fields ever need to match the baseline more closely, the
    // unit of work is the label positioning + `MuiOutlinedInput` TOGETHER,
    // verified against a regenerated `admin-configuration-visual` — not this
    // variant on its own.
  ],
};
