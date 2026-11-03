/**
 * The composer's mode picker, "Act ⌄": the mode the next turn starts in, as a
 * native pop-up button (`ComposerPicker`). Act edits with approvals, Plan
 * changes nothing, Bypass (a local thread only, `onBypassChange`) asks for no
 * approvals, Unattended (a local thread only, `onUnattendedChange`) asks for
 * no approvals and continues by itself under a grant; the current one carries
 * a check. Bypass keeps its first-time warning (`useBypassMode`) and
 * Unattended its grant dialog (`useUnattendedMode`): picking one asks the
 * page, which turns it on, and leaves plan mode, only once the person confirms.
 */
import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';

import { t } from '@/shared/i18n';
import { ComposerPicker } from '@/shared/ui/native-composer';
import { MenuRow, MenuRowHint, MenuRowIcon, MenuRowText } from '@/shared/ui/native-menu';

type ModeValue = 'act' | 'plan' | 'bypass' | 'unattended';

export interface ModePickerProps {
  planMode: boolean;
  onPlanModeChange: (planMode: boolean) => void;
  bypass: boolean;
  /** Offers Bypass (a local thread): `true` when it is picked, `false` when Act or Plan is. */
  onBypassChange: ((on: boolean) => void) | undefined;
  unattended?: boolean;
  /** Offers Unattended (a local thread): `true` when it is picked, `false` when another mode is. */
  onUnattendedChange?: ((on: boolean) => void) | undefined;
}

interface ModeOption {
  value: ModeValue;
  label: string;
  /** The row's accessible name: what the mode does, in a few words. */
  name: string;
  hint: string;
}

const modeValue = (planMode: boolean, bypass: boolean, unattended: boolean): ModeValue => {
  if (planMode) return 'plan';
  if (unattended) return 'unattended';
  return bypass ? 'bypass' : 'act';
};

function modeOptions(withBypass: boolean, withUnattended: boolean): ModeOption[] {
  const options: ModeOption[] = [
    { value: 'act', label: t('workspace.mode.act', 'Act'), name: t('workspace.actMode', 'Act mode (edits with approvals)'), hint: t('workspace.mode.actHint', 'Edits with approvals') },
    { value: 'plan', label: t('workspace.mode.plan', 'Plan'), name: t('workspace.planMode', 'Plan mode (no changes)'), hint: t('workspace.mode.planHint', 'Plans, changes nothing') },
  ];
  if (withBypass) {
    options.push({ value: 'bypass', label: t('workspace.mode.bypass', 'Bypass'), name: t('workspace.bypassMode', 'Bypass mode (no approvals)'), hint: t('workspace.mode.bypassHint', 'No approvals') });
  }
  if (withUnattended) {
    options.push({
      value: 'unattended',
      label: t('workspace.mode.unattended', 'Unattended'),
      name: t('workspace.unattendedMode', 'Unattended mode (continues by itself)'),
      hint: t('workspace.mode.unattendedHint', 'Continues by itself'),
    });
  }
  return options;
}

export function ModePicker({ planMode, onPlanModeChange, bypass, onBypassChange, unattended = false, onUnattendedChange }: ModePickerProps): React.JSX.Element {
  const options = modeOptions(onBypassChange !== undefined, onUnattendedChange !== undefined);
  const current = modeValue(planMode, bypass, unattended);
  // Bypass and Unattended turn plan mode off only once they are on (after the warning or the grant dialog): a cancelled dialog keeps the mode.
  const change = (next: ModeValue): void => {
    if (next === 'bypass' || next === 'unattended') {
      (next === 'bypass' ? onBypassChange : onUnattendedChange)?.(true);
      return;
    }
    onPlanModeChange(next === 'plan');
    onBypassChange?.(false);
    onUnattendedChange?.(false);
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
