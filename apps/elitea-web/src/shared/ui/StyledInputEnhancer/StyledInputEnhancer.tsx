import type { ReactNode } from 'react';
import { useCallback, useState } from 'react';

import Box from '@mui/material/Box';

import { BaseModal } from '../BaseModal';
import { CharacterCounter } from '../CharacterCounter';
import { InputBase, type InputBaseProps } from '../InputBase';
import { t } from '@/shared/i18n';

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface StyledInputEnhancerProps extends InputBaseProps {
  /** Modal title; falls back to the string `label`, then a generic default. */
  fullScreenTitle?: string;
}

interface FullScreenEditorProps {
  readonly value: InputBaseProps['value'];
  readonly onChange: InputBaseProps['onChange'];
  readonly ariaLabel: string;
  readonly htmlInputProps: (Record<string, unknown> & { maxLength?: number }) | undefined;
}

/** The modal's own content — split out purely to keep `StyledInputEnhancer` under this codebase's complexity gate (12). */
function FullScreenEditor({ value, onChange, ariaLabel, htmlInputProps }: FullScreenEditorProps): ReactNode {
  const maxLength = typeof htmlInputProps?.maxLength === 'number' ? htmlInputProps.maxLength : undefined;
  const stringValue = typeof value === 'string' ? value : '';
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
      <InputBase
        value={value}
        onChange={onChange}
        expand={{ minRows: 15, maxRows: 15 }}
        slotProps={{ htmlInput: { ...htmlInputProps, 'aria-label': ariaLabel } }}
        sx={{ flex: 1, minHeight: 0 }}
      />
      {maxLength !== undefined && (
        <CharacterCounter
          value={stringValue}
          maxLength={maxLength}
          data-testid="styled-input-enhancer-fullscreen-counter"
        />
      )}
    </Box>
  );
}

/**
 * `InputBase` plus a full-screen editing modal, wired to the toolbar's
 * full-screen action. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/input/StyledInputEnhancer.jsx`.
 *
 * The baseline rendered its own bespoke `StyledInputModal` (CodeMirror,
 * language detection, F-string autocomplete, variable-state options — none
 * of which exist in `shared/ui` yet). This composes the two components this
 * unit actually owns instead: `InputBase` for the field, `BaseModal`
 * (`variant="complex"`, `fullscreen`) hosting a second, larger `InputBase`
 * as the expanded editor. Both instances are controlled from the same
 * `value`/`onChange`, so typing in either place stays in sync.
 *
 * The toolbar defaults to `forceShow: true` (`InputBase`'s own default is
 * hover-only) — this component's entire purpose is the full-screen escape
 * hatch, so gating its one action behind a mouse hover would make it
 * undiscoverable for keyboard/touch users and untestable without simulating
 * one. A caller can still pass `actions={{ forceShow: false }}` to restore
 * hover-only behaviour.
 *
 * #895 — the full-screen modal's own `InputBase` now carries the SAME
 * `slotProps.htmlInput` a caller passed the collapsed field (so a
 * `maxLength` contract survives into full screen, aria-label still
 * overridden last), and a `CharacterCounter` renders beneath it whenever a
 * `maxLength` is present — the collapsed field's OWN counter is still the
 * caller's (`WelcomeMessageInput`/`ConversationStartersEditor`'s own JSX,
 * gated on focus); this one is unconditionally visible, matching a
 * full-screen editor having no adjacent "focused" cue to hide behind.
 */
export function StyledInputEnhancer({
  fullScreenTitle,
  label,
  value,
  onChange,
  actions,
  slotProps,
  ...rest
}: StyledInputEnhancerProps): ReactNode {
  const [open, setOpen] = useState(false);

  const handleOpen = useCallback(() => setOpen(true), []);
  const handleClose = useCallback(() => setOpen(false), []);

  const defaultTitle = t('shared.ui.styledInputEnhancer.title', 'Edit content');
  const modalTitle = fullScreenTitle ?? (typeof label === 'string' ? label : defaultTitle);
  const contentAriaLabel = typeof label === 'string' ? label : defaultTitle;

  const htmlInputProps = slotProps?.htmlInput as (Record<string, unknown> & { maxLength?: number }) | undefined;

  return (
    <>
      <InputBase
        label={label}
        value={value}
        onChange={onChange}
        actions={{
          ...actions,
          enabled: actions?.enabled ?? true,
          forceShow: actions?.forceShow ?? true,
          showFullScreen: true,
        }}
        onFullScreen={handleOpen}
        {...(slotProps !== undefined ? { slotProps } : {})}
        {...rest}
      />
      <BaseModal
        open={open}
        onClose={handleClose}
        title={modalTitle}
        variant="complex"
        fullscreen
        content={
          <FullScreenEditor
            value={value}
            onChange={onChange}
            ariaLabel={contentAriaLabel}
            htmlInputProps={htmlInputProps}
          />
        }
      />
    </>
  );
}
