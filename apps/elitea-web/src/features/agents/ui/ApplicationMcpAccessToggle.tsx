import type { ChangeEvent, ReactNode } from 'react';
import { useCallback } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseSwitch } from '@/shared/ui/BaseSwitch';
import { InfoLabelWithTooltip } from '@/shared/ui/InfoLabelWithTooltip';

export interface ApplicationMcpAccessToggleProps {
  readonly checked: boolean;
  readonly onChange: (checked: boolean) => void;
  readonly disabled?: boolean | undefined;
  readonly entityType: 'agent' | 'pipeline';
}

/**
 * Makes the existing application-version `mcp` tag contract explicit.
 *
 * Main still treats the exact tag name as the external MCP opt-in. This
 * control changes only that tag through its owning form; it does not add a
 * second exposure flag that could drift from the catalogue query.
 */
export function ApplicationMcpAccessToggle({
  checked,
  onChange,
  disabled = false,
  entityType,
}: ApplicationMcpAccessToggleProps): ReactNode {
  const handleChange = useCallback((event: ChangeEvent<HTMLInputElement>) => onChange(event.target.checked), [onChange]);
  const description = t(
    'agents.applicationMcpAccess.description',
    'Expose this {{entityType}} version as a tool through the project MCP endpoint.',
    { entityType },
  );

  return (
    <Box sx={rootSx}>
      <FormControlLabel
        control={
          <BaseSwitch
            checked={checked}
            onChange={handleChange}
            disabled={disabled}
            aria-label={t('agents.applicationMcpAccess.label', 'Enable MCP access')}
            data-testid={`${entityType}-mcp-access-toggle`}
          />
        }
        label={<InfoLabelWithTooltip label={t('agents.applicationMcpAccess.label', 'Enable MCP access')} tooltip={description} />}
      />
    </Box>
  );
}

const rootSx: SxProps<Theme> = (theme) => ({ marginTop: theme.spacing(2) });
