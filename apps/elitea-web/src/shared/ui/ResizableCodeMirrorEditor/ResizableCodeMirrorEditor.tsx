import type { ReactNode } from 'react';
import { useCallback, useEffect, useRef, useState } from 'react';

import FullscreenOutlinedIcon from '@mui/icons-material/FullscreenOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import type { Extension } from '@codemirror/state';

import { BaseModal } from '../BaseModal';
import { t } from '@/shared/i18n';
import { CodeMirrorEditor } from '../CodeMirrorEditor';

const DEFAULT_MIN_HEIGHT_PX = 120;
const DEFAULT_HEIGHT_PX = 200;

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface ResizableCodeMirrorEditorProps {
  value: string;
  /** Fires on blur with the edited value (see the component doc comment for why this, not per-keystroke, is the commit point). */
  onChange?: (value: string) => void;
  extensions?: Extension[];
  /** Pixel height floor for the drag-to-resize box. */
  minHeight?: number;
  /** Shows the fullscreen-expand button in the box's top-right corner. */
  expandAction?: boolean;
  // Explicit `| undefined`: callers commonly forward an already-optional
  // `meta.disabled`-shaped value straight through — see
  // `CodeMirrorEditorProps.readOnly`'s identical note.
  readOnly?: boolean | undefined;
  /** Fullscreen modal title, and this editor's accessible name. */
  fieldName?: string;
}

/**
 * A vertically drag-to-resize `CodeMirrorEditor`, with an optional fullscreen
 * expand button. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/field/ResizableCodeMirrorEditor.jsx`,
 * which composes `Field.CodeMirrorEditor` inside a resizable `Box` — that is
 * the actual relationship ported here too (`ResizableCodeMirrorEditor` is a
 * consumer of `CodeMirrorEditor`, not a ref-forwarding wrapper around it;
 * confirmed by reading both baseline files rather than assuming from the
 * component names alone).
 *
 * Commit semantics, preserved from the baseline: the box editor's own
 * `onChange` is NOT wired to this component's `onChange` — only `onBlur` is.
 * Typing updates local state and the visible document immediately, but the
 * caller (a tool-config form in the baseline) is only notified once the user
 * leaves the field. The baseline's fullscreen modal used a plain `<textarea>`
 * whose single `onChange` event both updated local state AND immediately
 * notified the caller (`handleChangeFromFullScreenModal`) — there was no
 * separate blur step to wait for in a native textarea's change handler. This
 * port's fullscreen editor is a second `CodeMirrorEditor`, so the same
 * "commit as you type" behaviour is reproduced via ITS `onChange` (debounced
 * ~30ms), not `onBlur`, while the box editor keeps the blur-commit contract.
 *
 * Baseline's `StyledInputModal` (`@/components/StyledInputModal`, the
 * fullscreen surface) is a top-level app component, not part of this unit's
 * port scope, and is not built in this app yet. The
 * fullscreen view here is `BaseModal` (`shared/ui/BaseModal`, already built)
 * in its `variant="complex"` `fullscreen` mode, hosting a second
 * `CodeMirrorEditor` sized to the modal instead — same user-facing behaviour
 * (a larger editing surface), built from an in-scope `shared/ui` primitive
 * instead of an unported one.
 *
 * Dropped vs. the baseline: `StyledInputModal`'s `specifiedLanguage="json"`
 * language-picker chrome (a dropdown to switch the fullscreen editor's
 * language) — that control lives on the modal component itself, not on
 * `ResizableCodeMirrorEditor`, and no in-scope caller of this component
 * passes anything that would need it (`extensions` already carries whatever
 * language support the caller wants, in both the box and the modal editor).
 */
export function ResizableCodeMirrorEditor({
  value,
  onChange,
  extensions,
  minHeight = DEFAULT_MIN_HEIGHT_PX,
  expandAction = false,
  readOnly = false,
  fieldName,
}: ResizableCodeMirrorEditorProps): ReactNode {
  const [currentValue, setCurrentValue] = useState(value);
  const [editorHeight, setEditorHeight] = useState(DEFAULT_HEIGHT_PX);
  const [fullScreenOpen, setFullScreenOpen] = useState(false);
  const resizeBoxRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    setCurrentValue(value);
  }, [value]);

  useEffect(() => {
    const target = resizeBoxRef.current;
    if (!target) return;

    const observer = new window.ResizeObserver((entries) => {
      const entry = entries[0];
      if (!entry) return;
      setEditorHeight(entry.contentRect.height);
    });
    observer.observe(target);

    return () => observer.disconnect();
  }, []);

  const handleChange = useCallback((newValue: string) => {
    setCurrentValue(newValue);
  }, []);

  const handleBlur = useCallback(
    (newValue: string) => {
      setCurrentValue(newValue);
      if (!readOnly) onChange?.(newValue);
    },
    [onChange, readOnly],
  );

  const handleFullScreenChange = useCallback(
    (newValue: string) => {
      setCurrentValue(newValue);
      if (!readOnly) onChange?.(newValue);
    },
    [onChange, readOnly],
  );

  const openFullScreen = useCallback(() => setFullScreenOpen(true), []);
  const closeFullScreen = useCallback(() => setFullScreenOpen(false), []);

  // `exactOptionalPropertyTypes` treats an explicit `extensions={undefined}`/
  // `aria-label={undefined}` as distinct from omitting the prop entirely —
  // conditional spreads (same idiom `BaseCheckbox` uses for its own optional
  // aria props) forward the value only when this component actually
  // received one, instead of forwarding `undefined` itself.
  const extensionsProp = extensions !== undefined ? { extensions } : {};
  const ariaLabelProp = fieldName !== undefined ? { 'aria-label': fieldName } : {};

  return (
    <>
      <Box
        ref={resizeBoxRef}
        data-testid="resizable-code-mirror-editor-box"
        // `overflow: 'auto'` below makes this a potentially-scrollable
        // region independent of CM6's own internal `.cm-scroller` (this box
        // can be smaller than its content via the CSS `resize` handle) —
        // axe's `scrollable-region-focusable` rule requires such a region
        // to be reachable and operable by keyboard on its own, not only by
        // mouse wheel/drag; `tabIndex={0}` makes it a real, focusable stop.
        tabIndex={0}
        sx={(theme: Theme) => ({
          boxSizing: 'border-box',
          position: 'relative',
          width: '100%',
          resize: 'vertical',
          overflow: 'auto',
          minHeight: `${minHeight}px`,
          height: `${DEFAULT_HEIGHT_PX}px`,
          border: `1px solid ${theme.vars.palette.border.table}`,
          borderRadius: theme.vars.shape.radiusSm,
          transition: theme.transitions.create('border-color'),
          '&:hover, &:focus-within': {
            borderColor: theme.vars.palette.border.hover,
          },
          '&:hover .resizable-code-mirror-editor-expand, &:focus-within .resizable-code-mirror-editor-expand':
            {
              opacity: 1,
            },
          '&:focus-within': { borderColor: theme.vars.palette.focus.ring },
        })}
      >
        {expandAction && (
          <Box
            sx={(theme: Theme) => ({
              position: 'absolute',
              top: theme.spacing(-0.25),
              right: theme.spacing(-0.25),
              zIndex: 1,
            })}
          >
            <Tooltip title={t('shared.ui.resizableCodeMirrorEditor.fullScreen', 'Full screen view')}>
              <IconButton
                className="resizable-code-mirror-editor-expand"
                color="tertiary"
                size="small"
                aria-label={t('shared.ui.resizableCodeMirrorEditor.fullScreen', 'Full screen view')}
                onClick={openFullScreen}
                sx={{
                  // Visually revealed on hover/focus-within (targeted
                  // above) or its own focus — `opacity`, not `visibility`
                  // or `display`: both of those remove an element from the
                  // accessibility tree entirely (the baseline's own
                  // `display: none` did exactly that, hiding the control
                  // from keyboard/AT users outright — R-C1 fix). `opacity:
                  // 0` keeps it perceivable and `Tab`-reachable at every
                  // moment; only its visual reveal is conditional.
                  opacity: 0,
                  '&:focus-visible': { opacity: 1 },
                }}
              >
                <FullscreenOutlinedIcon fontSize="small" />
              </IconButton>
            </Tooltip>
          </Box>
        )}
        <CodeMirrorEditor
          value={currentValue}
          height={`${editorHeight}px`}
          minHeight={`${minHeight}px`}
          onChange={handleChange}
          onBlur={handleBlur}
          readOnly={readOnly}
          {...extensionsProp}
          {...ariaLabelProp}
        />
      </Box>
      {expandAction && (
        <BaseModal
          open={fullScreenOpen}
          onClose={closeFullScreen}
          variant="complex"
          fullscreen
          title={fieldName}
          content={
            <CodeMirrorEditor
              value={currentValue}
              height="100%"
              minHeight="100%"
              onChange={handleFullScreenChange}
              readOnly={readOnly}
              {...extensionsProp}
              {...ariaLabelProp}
            />
          }
        />
      )}
    </>
  );
}
