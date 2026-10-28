/**
 * The composer's mode picker, "Act ⌄": the mode the next turn starts in, as a
 * native pop-up button (`ComposerPicker`). Act edits with approvals, Plan
 * changes nothing, Bypass (a local thread only, `onBypassChange`) asks for no
 * approvals; the current one carries a check. Bypass keeps its first-time
 * warning (`useBypassMode`): picking it asks the page, which turns it on, and
 * leaves plan mode, only once the warning is confirmed.
 */
import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';

import { t } from '@/shared/i18n';
import { ComposerPicker } from '@/shared/ui/native-composer';
import { MenuRow, MenuRowHint, MenuRowIcon, MenuRowText } from '@/shared/ui/native-menu';

type ModeValue = 'act' | 'plan' | 'bypass';

export interface ModePickerProps {
  planMode: boolean;
  onPlanModeChange: (planMode: boolean) => void;
  bypass: boolean;
  /** Offers Bypass (a local thread): `true` when it is picked, `false` when Act or Plan is. */
  onBypassChange: ((on: boolean) => void) | undefined;
}

interface ModeOption {
  value: ModeValue;
  label: string;
  /** The row's accessible name: what the mode does, in a few words. */
  name: string;
  hint: string;
}

const modeValue = (planMode: boolean, bypass: boolean): ModeValue => {
  if (planMode) return 'plan';
  return bypass ? 'bypass' : 'act';
};

function modeOptions(withBypass: boolean): ModeOption[] {
  const options: ModeOption[] = [
    { value: 'act', label: t('workspace.mode.act', 'Act'), name: t('workspace.actMode', 'Act mode (edits with approvals)'), hint: t('workspace.mode.actHint', 'Edits with approvals') },
    { value: 'plan', label: t('workspace.mode.plan', 'Plan'), name: t('workspace.planMode', 'Plan mode (no changes)'), hint: t('workspace.mode.planHint', 'Plans, changes nothing') },
  ];
  if (withBypass) {
    options.push({ value: 'bypass', label: t('workspace.mode.bypass', 'Bypass'), name: t('workspace.bypassMode', 'Bypass mode (no approvals)'), hint: t('workspace.mode.bypassHint', 'No approvals') });
  }
  return options;
}

export function ModePicker({ planMode, onPlanModeChange, bypass, onBypassChange }: ModePickerProps): React.JSX.Element {
  const options = modeOptions(onBypassChange !== undefined);
  const current = modeValue(planMode, bypass);
  // Bypass turns plan mode off only once it is on (after its warning, the first time): a cancelled warning keeps the mode.
  const change = (next: ModeValue): void => {
    if (next === 'bypass') {
      onBypassChange?.(true);
      return;
    }
    onPlanModeChange(next === 'plan');
    onBypassChange?.(false);
  };
  return (
    <ComposerPicker label={t('workspace.mode', 'Mode')} value={options.find((option) => option.value === current)?.label ?? ''} testId="mode-picker" minMenuWidth="15rem">
      {(close) =>
        options.map((option) => (
          <MenuRow
            key={option.value}
            role="menuitemradio"
            aria-checked={option.value === current}
            aria-label={option.name}
            data-testid={`mode-${option.value}`}
            onClick={() => {
              close();
              change(option.value);
            }}
          >
            <MenuRowIcon>{option.value === current && <CheckOutlinedIcon />}</MenuRowIcon>
            <MenuRowText>{option.label}</MenuRowText>
            <MenuRowHint>{option.hint}</MenuRowHint>
          </MenuRow>
        ))
      }
    </ComposerPicker>
  );
}
