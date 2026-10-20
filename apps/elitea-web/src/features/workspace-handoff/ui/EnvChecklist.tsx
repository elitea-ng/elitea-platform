/**
 * The environment variables a send could carry, as a searchable checklist:
 * the name and how long its value is (`•••••• (23 chars)`). The UI never has
 * a value: the host sends names and lengths only, and reads the values again
 * at send.
 */
import { useState } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { EnvVar } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

export interface EnvChecklistProps {
  offered: readonly EnvVar[];
  selected: readonly string[];
  onToggle: (name: string) => void;
  disabled: boolean;
}

/** What stands in for a value: its length only. */
function maskedValue(length: number): string {
  return t('workspace.handoff.env.masked', '•••••• ({{n}} chars)', { n: String(length) });
}

export function EnvChecklist({ offered, selected, onToggle, disabled }: EnvChecklistProps): React.JSX.Element {
  const [search, setSearch] = useState('');
  const needle = search.trim().toLowerCase();
  const shown = needle === '' ? offered : offered.filter((env) => env.name.toLowerCase().includes(needle));
  return (
    <Box data-testid="handoff-env-list" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <TextField
        size="small"
        label={t('workspace.handoff.env.search', 'Search variables')}
        value={search}
        onChange={(event) => setSearch(event.target.value)}
      />
      {offered.length === 0 && (
        <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary })}>
          {t('workspace.handoff.env.none', 'No environment variables can be sent from this computer.')}
        </Typography>
      )}
      <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none', maxHeight: '12rem', overflowY: 'auto' }}>
        {shown.map((env) => (
          <li key={env.name}>
            <FormControlLabel
              control={<BaseCheckbox size="small" checked={selected.includes(env.name)} disabled={disabled} onChange={() => onToggle(env.name)} />}
              label={
                <Typography variant="bodySmall" component="span">
                  <Box component="span" sx={{ fontFamily: 'monospace' }}>
                    {env.name}
                  </Box>
                  {` ${maskedValue(env.length)}`}
                </Typography>
              }
            />
          </li>
        ))}
      </Box>
    </Box>
  );
}
