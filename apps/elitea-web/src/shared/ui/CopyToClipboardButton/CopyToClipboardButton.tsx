import type { ReactNode } from 'react';
import { useCallback } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { handleCopy } from '@/shared/lib/clipboard';

import { BaseBtn } from '../BaseBtn';

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface CopyToClipboardButtonProps {
  label: ReactNode;
  value: string;
  tooltip?: ReactNode;
  /** Called after the value has been copied — a caller wanting a "Copied!" toast fires it from here. */
  onCopied?: () => void;
  'data-testid'?: string;
}

/**
 * A labelled value with a click-to-copy button. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/button/CopyToClipboardButton.jsx`.
 * Colour/geometry come from `shared/brand/mui-overrides/MuiButton.ts`'s
 * `elitea`/`tertiary` entries (unit S1 Part B) — this component owns no
 * `styled()`/variant styling of its own.
 *
 * DEPENDENCY-INJECTION DEVIATION (deliberate, documented): the baseline
 * calls `useToast()` (a features-layer hook) directly from inside this
 * component. `shared/ui` cannot import from `features/` (layer rule R-L1),
 * so this takes an `onCopied?: () => void` callback instead — a caller that
 * wants a toast fires it from there. The clipboard write itself uses
 * `shared/lib/clipboard`'s `handleCopy` (the same helper unit S3 ported from
 * the baseline's own `utils.jsx`/`browserUtils.js`), not a raw
 * `navigator.clipboard` call, so the async-Clipboard-API-with-fallback
 * behaviour is shared with every other copy-to-clipboard call site.
 */
export function CopyToClipboardButton({
  label,
  value,
  tooltip,
  onCopied,
  'data-testid': dataTestId,
}: CopyToClipboardButtonProps): ReactNode {
  const onClick = useCallback(() => {
    void handleCopy(value).then(() => onCopied?.());
  }, [value, onCopied]);

  const button = (
    <BaseBtn
      variant="elitea"
      color="tertiary"
      onClick={onClick}
      data-testid={dataTestId}
    >
      <Typography
        variant="bodyMedium"
        sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary })}
      >
        {value}
      </Typography>
    </BaseBtn>
  );

  return (
    <Box
      sx={{
        display: 'flex',
        flexDirection: 'row',
        alignItems: 'center',
        gap: '0.25rem',
      }}
    >
      <Typography variant="bodyMedium">{label}</Typography>
      {tooltip !== undefined ? (
        // `describeChild`: the button already has a visible label (the value),
        // so the tooltip DESCRIBES it (aria-describedby) instead of replacing
        // its name with an aria-label. Without it the accessible name
        // ("Copy to clipboard") does not contain the visible text, which is
        // WCAG 2.5.3 Label in Name (axe `label-content-name-mismatch`): a
        // voice-control user who says what they see cannot reach the button.
        <Tooltip
          title={tooltip}
          placement="top"
          describeChild
        >
          {button}
        </Tooltip>
      ) : (
        button
      )}
    </Box>
  );
}
