import { useState, type ReactNode } from 'react';

import OpenInFullOutlinedIcon from '@mui/icons-material/OpenInFullOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';
import { CodeMirrorEditor } from '@/shared/ui/CodeMirrorEditor';

/**
 * Opens the Instructions field on a near-fullscreen surface.
 *
 * The reference gives every long-text agent field an expand affordance
 * (`[fsd]/shared/ui/input/StyledInputEnhancer.jsx`'s `showFullScreenButton`,
 * and the fullscreen dialog behind it). This app had none, so an agent's
 * instructions — the longest field on the page — could only ever be edited
 * inside an 8rem editor box, and the legacy suite's fullscreen cases
 * (`agents/test_agent_character_limits.py`) had nothing to open.
 *
 * A separate file, not eight more lines inside `InstructionsInput.tsx`: that
 * file is at 393 of its 400-line §3.5 budget.
 *
 * The dialog edits the SAME value through the SAME `onChange` the inline
 * editor uses. It holds no draft of its own, so closing it is not a discard
 * and there is no second copy of the text to reconcile — the failure mode a
 * dialog with its own state would add.
 *
 * The mention pickers ("/" tools, "~" skills) are deliberately NOT wired into
 * the fullscreen editor. They are driven by CodeMirror extensions bound to the
 * inline editor's own view (`InstructionsInput.tsx`'s bridge), and a second
 * view would need a second set of hook instances writing to one value. The
 * dialog is for reading and writing long text; the pickers stay where they
 * work.
 */
export interface InstructionsFullscreenButtonProps {
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly disabled?: boolean | undefined;
}

export function InstructionsFullscreenButton({ value, onChange, disabled }: InstructionsFullscreenButtonProps): ReactNode {
  const [open, setOpen] = useState(false);

  return (
    <Box sx={wrapperSx}>
      <Tooltip title={t('features.agents.instructionsFullscreen.open', 'Open in full screen')}>
        <IconButton
          size="small"
          aria-label={t('features.agents.instructionsFullscreen.open', 'Open in full screen')}
          data-testid="agent-instructions-fullscreen-button"
          onClick={() => setOpen(true)}
        >
          <OpenInFullOutlinedIcon fontSize="small" />
        </IconButton>
      </Tooltip>
      <BaseModal
        open={open}
        variant="complex"
        fullscreen
        onClose={() => setOpen(false)}
        title={t('features.agents.instructionsFullscreen.title', 'Instructions')}
        data-testid="agent-instructions-fullscreen-dialog"
        content={
          <CodeMirrorEditor
            value={value}
            onChange={onChange}
            readOnly={disabled}
            minHeight="60vh"
            aria-label={t('features.agents.instructionsFullscreen.editorAriaLabel', 'Instructions, full screen')}
          />
        }
      />
    </Box>
  );
}

const wrapperSx: SxProps<Theme> = { display: 'flex', justifyContent: 'flex-end' };
