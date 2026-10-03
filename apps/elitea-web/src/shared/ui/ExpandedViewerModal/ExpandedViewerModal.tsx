import type { ReactNode, RefObject } from 'react';
import { useEffect, useRef, useState } from 'react';

// [S1-H] Interim icon, same rationale `BaseModal`'s own doc comment records
// for `CloseIcon`: the baseline's `CopyIcon` is a hand-rolled
// `@/components/Icons/CopyIcon`, not one of the custom SVGs ported into
// `shared/ui/icons/` (no `copy-icon.tsx`/`content-copy-icon.tsx` exists
// there). `ContentCopy` is the standard `@mui/icons-material` equivalent,
// used the same fallback way `BaseModal` uses `Close` and the baseline's own
// `SecretField.jsx` uses `Visibility`/`VisibilityOff` directly.
// TODO(follow-up): swap for a ported `shared/ui/icons` copy glyph once one
// lands, for pixel-parity with the baseline's outline icon.
import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import Box from '@mui/material/Box';
import FormControl from '@mui/material/FormControl';
import IconButton from '@mui/material/IconButton';
import MenuItem from '@mui/material/MenuItem';
import Select, { type SelectChangeEvent } from '@mui/material/Select';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { BaseModal } from '../BaseModal';
import { t } from '@/shared/i18n';

const TITLE_TRUNCATION_CHECK_DELAY_MS = 100;

/** @public One entry in {@link ExpandedViewerModalLanguageOptions.options}. */
export interface ExpandedViewerModalLanguageOption {
  label: string;
  value: string;
}

/** @public Content-type selector shown in the header, `variant="complex"` territory. Omit entirely to hide the selector. */
export interface ExpandedViewerModalLanguageOptions {
  value?: string;
  options: ExpandedViewerModalLanguageOption[];
  onChange?: (value: string) => void;
  disabled?: boolean;
}

/** @public Header-row extras beyond the language selector. */
export interface ExpandedViewerModalHeaderOptions {
  /** Shows the copy button when supplied. The caller owns the actual clipboard write (and any toast) — see the component doc comment. */
  onCopy?: () => void;
  customButtons?: ReactNode;
  closeButtonDataTestId?: string;
}

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface ExpandedViewerModalProps {
  open: boolean;
  onClose?: () => void;
  /** String titles get the truncation-aware tooltip treatment below; pass a node to opt out. */
  title: ReactNode;
  content?: ReactNode;
  footer?: ReactNode;
  language?: ExpandedViewerModalLanguageOptions;
  header?: ExpandedViewerModalHeaderOptions;
  'data-testid'?: string;
}

/**
 * Tracks whether a string title is visually truncated (so the tooltip only
 * shows when there is actually more text to reveal). Extracted to a hook —
 * not inlined in the component — so its one `useEffect` is attributed to
 * this hook by `scripts/check-budgets.mjs`'s AST walk, not to
 * `ExpandedViewerModal` (the tool's own suggested fix for the §3.5
 * use-effects-per-component budget is "extract hooks").
 */
function useTitleTruncation(
  open: boolean,
  title: ReactNode,
): { titleRef: RefObject<HTMLElement | null>; isTruncated: boolean } {
  const titleRef = useRef<HTMLElement | null>(null);
  const [isTruncated, setIsTruncated] = useState(false);

  useEffect(() => {
    if (!open) return;
    const checkTruncation = (): void => {
      const el = titleRef.current;
      if (el) setIsTruncated(el.scrollWidth > el.clientWidth);
    };
    const timeoutId = window.setTimeout(checkTruncation, TITLE_TRUNCATION_CHECK_DELAY_MS);
    window.addEventListener('resize', checkTruncation);
    return () => {
      window.clearTimeout(timeoutId);
      window.removeEventListener('resize', checkTruncation);
    };
  }, [open, title]);

  return { titleRef, isTruncated };
}

interface ModalTitleProps {
  title: ReactNode;
  titleRef: RefObject<HTMLElement | null>;
  isTruncated: boolean;
}

/** String titles render through a truncation-aware `Typography` + `Tooltip`; any other node passes through untouched (same escape hatch `BaseModal.title` documents). */
function ModalTitle({ title, titleRef, isTruncated }: ModalTitleProps): ReactNode {
  if (typeof title !== 'string') return title;
  return (
    <Tooltip
      title={isTruncated ? title : ''}
      placement="top"
    >
      <Typography
        ref={titleRef}
        variant="headingMedium"
        component="span"
        color="text.secondary"
        // A block span: the title sits inside BaseModal's `<h2>`, so it must not
        // be a heading itself, and ellipsis needs a block box.
        sx={{ display: 'block', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', maxWidth: '100%' }}
      >
        {title}
      </Typography>
    </Tooltip>
  );
}

interface LanguageSelectProps {
  language: ExpandedViewerModalLanguageOptions;
}

/**
 * The baseline's `CodeMirrorEditorHelpers.languageOptions` + `SingleSelect`
 * combo is entity-level knowledge `shared/ui` cannot import (layer rule
 * R-L1) and a component that does not exist here yet. Redesigned as a plain
 * MUI `Select` driven entirely by `language.options` the caller supplies —
 * same "no domain-specific option source inside shared/ui" fix unit S7
 * already applied to `CronPresetSelect`/`CronFieldEditor`.
 */
function LanguageSelect({ language }: LanguageSelectProps): ReactNode {
  const contentTypeLabel = t('shared.ui.expandedViewerModal.contentType', 'Content type');

  const handleChange = (event: SelectChangeEvent<string>): void => {
    language.onChange?.(event.target.value);
  };

  return (
    <Box
      sx={{ display: 'flex', alignItems: 'center', gap: (theme: Theme) => theme.spacing(1) }}
    >
      <Typography
        variant="bodyMedium"
        color="text.default"
      >
        {t('shared.ui.expandedViewerModal.contentTypeLabel', 'Content type:')}
      </Typography>
      <FormControl
        variant="standard"
        size="small"
        disabled={language.disabled}
        sx={{ minWidth: '7rem' }}
      >
        <Select<string>
          aria-label={contentTypeLabel}
          value={language.value ?? ''}
          onChange={handleChange}
        >
          {language.options.map((option) => (
            <MenuItem
              key={option.value}
              value={option.value}
            >
              {option.label}
            </MenuItem>
          ))}
        </Select>
      </FormControl>
    </Box>
  );
}

interface CopyButtonProps {
  onCopy: () => void;
}

function CopyButton({ onCopy }: CopyButtonProps): ReactNode {
  const copyLabel = t('shared.ui.expandedViewerModal.copyTooltip', 'Copy to clipboard');
  return (
    <Tooltip
      title={copyLabel}
      placement="top"
    >
      <IconButton
        aria-label={copyLabel}
        size="small"
        onClick={onCopy}
      >
        <ContentCopyIcon fontSize="small" />
      </IconButton>
    </Tooltip>
  );
}

/**
 * A `BaseModal`-composed fullscreen viewer: truncation-aware title, an
 * optional content-type selector + copy button in the header, and caller
 * content. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/modal/ExpandedViewerModal.jsx`.
 *
 * Deviations from the baseline:
 *  - `specifiedLanguage`/`useLanguageLinter`'s internal fallback, `Copy to
 *    clipboard` via `handleCopy` + `useToast`, and the CodeMirror-domain
 *    language list are all app/entity-level concerns `shared/ui` cannot
 *    import (layer rule R-L1: no `features/`/`entities/`/Redux/app-context).
 *    Redesigned so the caller owns all three: `language` is a fully
 *    controlled option group (`value`/`options`/`onChange`), and
 *    `header.onCopy` is a plain callback — this component renders the
 *    button but performs no clipboard write and shows no toast itself.
 *  - The baseline's `sx`/`dialogSx`/`contentBackgroundSx` props styled
 *    `BaseModal`'s own paper/content slots directly. `BaseModal` (S1, not
 *    touched by this unit) exposes no `sx` passthrough for either slot, so
 *    that customization surface is dropped; a caller needing different
 *    content-area styling should style their own `content` node instead.
 *  - `StyledTooltip`/`SingleSelect` (not yet ported to `shared/ui`) are
 *    replaced with plain MUI `Tooltip`/`Select`, matching the precedent
 *    unit S7 set in `shared/ui/cron/` for the same "no legacy wrapper,
 *    plain MUI primitive" reasoning.
 */
export function ExpandedViewerModal({
  open,
  onClose,
  title,
  content,
  footer,
  language,
  header,
  'data-testid': dataTestId,
}: ExpandedViewerModalProps): ReactNode {
  const { titleRef, isTruncated } = useTitleTruncation(open, title);

  const headerActions = (
    <>
      {language && <LanguageSelect language={language} />}
      {header?.customButtons}
      {header?.onCopy && <CopyButton onCopy={header.onCopy} />}
    </>
  );

  return (
    <BaseModal
      open={open}
      variant="complex"
      fullscreen
      title={
        <ModalTitle
          title={title}
          titleRef={titleRef}
          isTruncated={isTruncated}
        />
      }
      header={{
        actions: headerActions,
        ...(header?.closeButtonDataTestId !== undefined
          ? { closeButtonDataTestId: header.closeButtonDataTestId }
          : {}),
      }}
      content={content}
      footer={footer}
      {...(onClose !== undefined ? { onClose } : {})}
      {...(dataTestId !== undefined ? { 'data-testid': dataTestId } : {})}
    />
  );
}
