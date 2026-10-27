/**
 * The step limit in the composer's ⋯ menu: the model steps each run of this
 * folder's turns may take (the workspace's agent settings, IPC.md "Step
 * limit"). The row "Step limit" shows the current value; picking it turns
 * the menu into the choices: the default, a few presets, unlimited and a
 * number of your own. `/steps` reads and sets the same setting; both stay in
 * step (`onStepLimitChange`). At the limit the host asks whether to continue.
 * Loaded lazily by `ThreadComposer`, for a local thread only.
 */
import { useEffect, useState, type SyntheticEvent } from 'react';

import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';
import ChevronLeftIcon from '@mui/icons-material/ChevronLeft';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import SpeedOutlinedIcon from '@mui/icons-material/SpeedOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import TextField from '@mui/material/TextField';

import { describeWorkspaceError } from '@/features/workspace';
import { DEFAULT_STEP_LIMIT, MAX_STEP_LIMIT, onStepLimitChange, parseStepCount, readStepLimit, UNLIMITED, writeStepLimit, type StepLimit } from '@/shared/desktop/stepLimit';
import { t } from '@/shared/i18n';
import { MenuDivider, MenuNote, MenuRow, MenuRowHint, MenuRowIcon, MenuRowText } from '@/shared/ui/native-menu';

/** The presets the menu offers between the default and unlimited. */
const PRESETS: readonly number[] = [100, 500, 2000, 5000];

/** "1024 steps", "∞ steps": what the row shows for `limit` (`null`: the default). */
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
        sx={{ width: '7.5rem' }}
      />
      <Button type="submit" size="small" variant="contained" disabled={steps === null} data-testid="steps-chip-custom-set">
        {t('workspace.stepsChip.set', 'Set')}
      </Button>
    </Box>
  );
}

export interface StepLimitMenuProps {
  workspaceId: string;
  onError: (message: string) => void;
  /** Closes the ⋯ menu. */
  close: () => void;
}

/** The ⋯ menu's step-limit rows: "Step limit  1024 steps ›", or, once it is picked, the choices. */
export default function StepLimitMenu({ workspaceId, onError, close }: StepLimitMenuProps): React.JSX.Element[] | null {
  const limit = useStepLimit(workspaceId);
  const [choosing, setChoosing] = useState(false);
  const [custom, setCustom] = useState(false);
  if (limit === undefined) return null;
  const label = stepsLabel(limit);
  if (!choosing) {
    return [
      <MenuRow key="steps" data-testid="steps-chip" aria-haspopup="menu" aria-label={t('workspace.stepsChip.aria', 'Step limit: {{label}}', { label })} onClick={() => setChoosing(true)}>
        <MenuRowIcon>
          <SpeedOutlinedIcon />
        </MenuRowIcon>
        <MenuRowText>{t('workspace.stepsChip.menu', 'Step limit')}</MenuRowText>
        <MenuRowHint>{label}</MenuRowHint>
        <MenuRowIcon>
          <ChevronRightIcon />
        </MenuRowIcon>
      </MenuRow>,
    ];
  }
  const choose = (next: StepLimit | null): void => {
    close();
    writeStepLimit(workspaceId, next).catch((error: unknown) => onError(describeWorkspaceError(error)));
  };
  const item = (value: StepLimit | null, text: string): React.JSX.Element => (
    <MenuRow key={String(value)} selected={value === limit} onClick={() => choose(value)} data-testid={`steps-chip-${String(value ?? 'default')}`}>
      <MenuRowIcon>{value === limit && <CheckOutlinedIcon />}</MenuRowIcon>
      <MenuRowText>{text}</MenuRowText>
    </MenuRow>
  );
  return [
    <MenuRow key="back" onClick={() => setChoosing(false)}>
      <MenuRowIcon>
        <ChevronLeftIcon />
      </MenuRowIcon>
      <MenuRowText>{t('workspace.stepsChip.menu', 'Step limit')}</MenuRowText>
    </MenuRow>,
    item(null, t('workspace.stepsChip.default', 'Default ({{steps}})', { steps: String(DEFAULT_STEP_LIMIT) })),
    ...PRESETS.map((steps) => item(steps, String(steps))),
    item(UNLIMITED, t('workspace.stepsChip.unlimitedItem', 'Unlimited')),
    <MenuDivider key="divider" />,
    custom ? (
      <CustomSteps key="custom" initial={typeof limit === 'number' ? String(limit) : ''} onSet={choose} />
    ) : (
      <MenuRow key="custom" onClick={() => setCustom(true)} data-testid="steps-chip-custom">
        <MenuRowIcon />
        <MenuRowText>{t('workspace.stepsChip.custom', 'Custom…')}</MenuRowText>
      </MenuRow>
    ),
    <MenuNote key="note">{t('workspace.stepsChip.tooltip', 'Model steps per run here. At the limit the agent asks whether to continue.')}</MenuNote>,
  ];
}
