/**
 * The composer footer's step-limit chip: the model steps each run of this
 * folder's turns may take (the workspace's agent settings, IPC.md "Step
 * limit"), with a menu of the default, a few presets, unlimited and a number
 * of your own. `/steps` reads and sets the same setting; both stay in step
 * (`onStepLimitChange`). At the limit the host asks whether to continue.
 * Loaded lazily by `ThreadComposer`, for a local thread only.
 */
import { useEffect, useState, type SyntheticEvent } from 'react';

import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import ButtonBase from '@mui/material/ButtonBase';
import Divider from '@mui/material/Divider';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Tooltip from '@mui/material/Tooltip';

import { describeWorkspaceError } from '@/features/workspace';
import { DEFAULT_STEP_LIMIT, MAX_STEP_LIMIT, onStepLimitChange, parseStepCount, readStepLimit, UNLIMITED, writeStepLimit, type StepLimit } from '@/shared/desktop/stepLimit';
import { t } from '@/shared/i18n';

/** The presets the menu offers between the default and unlimited. */
const PRESETS: readonly number[] = [100, 500, 2000, 5000];

const chipSx = (theme: Theme) => ({
  minHeight: 22,
  gap: 0.5,
  padding: '0 6px',
  borderRadius: '6px',
  fontSize: '0.75rem',
  fontWeight: 500,
  color: theme.vars.palette.text.secondary,
  whiteSpace: 'nowrap',
  '&:hover, &[aria-expanded="true"]': { background: `color-mix(in srgb, ${theme.vars.palette.text.primary} 8%, transparent)` },
  '& svg': { fontSize: 14 },
});

/** "1024 steps", "∞ steps": what the chip shows for `limit` (`null`: the default). */
function stepsLabel(limit: StepLimit | null): string {
  if (limit === UNLIMITED) return t('workspace.stepsChip.unlimited', '∞ steps');
  return t('workspace.stepsChip.label', '{{steps}} steps', { steps: String(limit ?? DEFAULT_STEP_LIMIT) });
}

/** The workspace's limit, kept current by `/steps` and the menu; `undefined` until the host answered (or outside it). */
function useStepLimit(workspaceId: string): StepLimit | null | undefined {
  const [limit, setLimit] = useState<StepLimit | null | undefined>(undefined);
  useEffect(() => {
    let live = true;
    readStepLimit(workspaceId).then(
      (read) => {
        if (live) setLimit(read);
      },
      () => undefined,
    );
    const stop = onStepLimitChange((changed, stored) => {
      if (changed === workspaceId) setLimit(stored);
    });
    return () => {
      live = false;
      stop();
    };
  }, [workspaceId]);
  return limit;
}

/** "Custom…": a number of your own, checked before it is sent. */
function CustomSteps({ initial, onSet }: { initial: string; onSet: (steps: number) => void }): React.JSX.Element {
  const [text, setText] = useState(initial);
  const steps = parseStepCount(text);
  const submit = (event: SyntheticEvent): void => {
    event.preventDefault();
    if (steps !== null) onSet(steps);
  };
  return (
    <Box component="form" onSubmit={submit} sx={{ display: 'flex', gap: 1, alignItems: 'flex-start', px: 1.5, py: 1 }}>
      <TextField
        size="small"
        value={text}
        onChange={(event) => setText(event.target.value)}
        // The menu's type-ahead would take the keys.
        onKeyDown={(event) => event.stopPropagation()}
        error={text !== '' && steps === null}
        helperText={text !== '' && steps === null ? t('workspace.stepsChip.invalid', '1 to {{max}}', { max: String(MAX_STEP_LIMIT) }) : ' '}
        slotProps={{ htmlInput: { inputMode: 'numeric', 'aria-label': t('workspace.stepsChip.customLabel', 'Steps per run'), 'data-testid': 'steps-chip-custom-input' } }}
        sx={{ width: 120 }}
      />
      <Button type="submit" size="small" variant="contained" disabled={steps === null} data-testid="steps-chip-custom-set">
        {t('workspace.stepsChip.set', 'Set')}
      </Button>
    </Box>
  );
}

export default function StepsChip({ workspaceId, onError }: { workspaceId: string; onError: (message: string) => void }): React.JSX.Element | null {
  const limit = useStepLimit(workspaceId);
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const [custom, setCustom] = useState(false);
  if (limit === undefined) return null;
  const close = (): void => {
    setAnchor(null);
    setCustom(false);
  };
  const choose = (next: StepLimit | null): void => {
    close();
    writeStepLimit(workspaceId, next).catch((error: unknown) => onError(describeWorkspaceError(error)));
  };
  const label = stepsLabel(limit);
  const item = (value: StepLimit | null, text: string) => (
    <MenuItem key={String(value)} selected={value === limit} onClick={() => choose(value)} data-testid={`steps-chip-${String(value ?? 'default')}`}>
      {text}
    </MenuItem>
  );
  return (
    <>
      <Tooltip title={t('workspace.stepsChip.tooltip', 'Model steps per run here. At the limit the agent asks whether to continue.')}>
        <ButtonBase
          data-testid="steps-chip"
          aria-haspopup="menu"
          aria-expanded={anchor !== null}
          aria-label={t('workspace.stepsChip.aria', 'Step limit: {{label}}', { label })}
          onClick={(event) => setAnchor(event.currentTarget)}
          sx={chipSx}
        >
          {label}
          <ExpandMoreIcon />
        </ButtonBase>
      </Tooltip>
      <Menu
        anchorEl={anchor}
        open={anchor !== null}
        onClose={close}
        anchorOrigin={{ vertical: 'top', horizontal: 'left' }}
        transformOrigin={{ vertical: 'bottom', horizontal: 'left' }}
        slotProps={{ list: { dense: true, 'aria-label': t('workspace.stepsChip.menu', 'Step limit') } }}
      >
        {item(null, t('workspace.stepsChip.default', 'Default ({{steps}})', { steps: String(DEFAULT_STEP_LIMIT) }))}
        {PRESETS.map((steps) => item(steps, String(steps)))}
        {item(UNLIMITED, t('workspace.stepsChip.unlimitedItem', 'Unlimited'))}
        <Divider />
        {custom ? (
          <CustomSteps initial={typeof limit === 'number' ? String(limit) : ''} onSet={choose} />
        ) : (
          <MenuItem onClick={() => setCustom(true)} data-testid="steps-chip-custom">
            {t('workspace.stepsChip.custom', 'Custom…')}
          </MenuItem>
        )}
      </Menu>
    </>
  );
}
