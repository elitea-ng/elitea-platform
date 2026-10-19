/**
 * HttpActionRulesTable — the project HTTP action rules list (T-5D).
 *
 * A plain MUI `Table`, like `WebhooksTable`. A caller with
 * `models.http_action_rules.manage` gets every column, the enabled switch and
 * Edit. A caller with only `models.http_action_rules.list` gets names and the
 * enabled state, read-only: the names route never returns origins, methods or
 * credentials, so there is nothing else to show.
 */
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import IconButton from '@mui/material/IconButton';
import Paper from '@mui/material/Paper';
import Switch from '@mui/material/Switch';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import type { HttpActionRule, HttpActionRuleName } from '@/shared/api/generated/model';
import { monoFontFamily } from '@/shared/brand/typeScale';

export interface HttpActionRulesTableProps {
  readonly isLoading: boolean;
  /** Full rules (manage). When present, `names` is ignored. */
  readonly rules?: readonly HttpActionRule[] | undefined;
  /** Names-only rows (list permission), rendered read-only. */
  readonly names?: readonly HttpActionRuleName[] | undefined;
  readonly onEdit?: ((rule: HttpActionRule) => void) | undefined;
  readonly onToggleEnabled?: ((rule: HttpActionRule, enabled: boolean) => void) | undefined;
}

const tableRootSx: SxProps<Theme> = { width: '100%' };
const emptyMessageSx: SxProps<Theme> = { padding: '1.5rem', textAlign: 'center', color: 'text.secondary' };
const endpointSx: SxProps<Theme> = {
  fontFamily: monoFontFamily,
  maxWidth: '20rem',
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

function enabledLabel(enabled: boolean): string {
  return enabled
    ? t('entities.httpActionRule.table.enabledState', 'Enabled')
    : t('entities.httpActionRule.table.disabledState', 'Disabled');
}

function replayLabel(replay: HttpActionRule['replay']): string {
  return replay === 'idempotency_key'
    ? t('entities.httpActionRule.replay.idempotencyKey', 'Idempotency key')
    : t('entities.httpActionRule.replay.never', 'Never');
}

function RuleRow({ rule, onEdit, onToggleEnabled }: { rule: HttpActionRule } & Pick<HttpActionRulesTableProps, 'onEdit' | 'onToggleEnabled'>) {
  const endpoint = `${rule.origin}${rule.path_template}`;
  return (
    <TableRow data-testid={`http-action-rule-row-${rule.id}`}>
      <TableCell>{rule.name}</TableCell>
      <TableCell>{rule.method}</TableCell>
      <TableCell>
        <Typography variant="bodySmall" component="div" sx={endpointSx} title={endpoint}>
          {endpoint}
        </Typography>
      </TableCell>
      <TableCell>
        {rule.configuration_id === 0 ? t('entities.httpActionRule.table.noCredential', 'None') : rule.configuration_id}
      </TableCell>
      <TableCell>{replayLabel(rule.replay)}</TableCell>
      <TableCell>
        <Switch
          checked={rule.enabled}
          onChange={(event) => onToggleEnabled?.(rule, event.target.checked)}
          slotProps={{ input: { 'aria-label': t('entities.httpActionRule.table.toggleEnabledAria', 'Toggle enabled') } }}
          data-testid={`http-action-rule-enabled-toggle-${rule.id}`}
        />
      </TableCell>
      <TableCell align="right">
        <Tooltip title={t('entities.httpActionRule.table.edit', 'Edit')}>
          <IconButton
            size="small"
            onClick={() => onEdit?.(rule)}
            aria-label={t('entities.httpActionRule.table.editAria', 'Edit HTTP action rule')}
            data-testid={`http-action-rule-edit-${rule.id}`}
          >
            <EditOutlinedIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      </TableCell>
    </TableRow>
  );
}

export function HttpActionRulesTable({ isLoading, rules, names, onEdit, onToggleEnabled }: HttpActionRulesTableProps) {
  const rowCount = rules ? rules.length : (names?.length ?? 0);
  if (!isLoading && rowCount === 0) {
    return (
      <Paper elevation={0} sx={emptyMessageSx} data-testid="http-action-rules-empty">
        <Typography variant="bodyMedium">
          {t('entities.httpActionRule.table.empty', 'No HTTP action rules yet. A pipeline HTTP node can only call a rule defined here.')}
        </Typography>
      </Paper>
    );
  }

  return (
    <TableContainer component={Paper} elevation={0} sx={tableRootSx}>
      <Table size="small" data-testid="http-action-rules-table">
        <TableHead>
          <TableRow>
            <TableCell>{t('entities.httpActionRule.table.name', 'Name')}</TableCell>
            {rules && (
              <>
                <TableCell>{t('entities.httpActionRule.table.method', 'Method')}</TableCell>
                <TableCell>{t('entities.httpActionRule.table.endpoint', 'Endpoint')}</TableCell>
                <TableCell>{t('entities.httpActionRule.table.credential', 'Credential')}</TableCell>
                <TableCell>{t('entities.httpActionRule.table.replay', 'Replay')}</TableCell>
              </>
            )}
            <TableCell>{t('entities.httpActionRule.table.enabled', 'Enabled')}</TableCell>
            {rules && <TableCell align="right">{t('entities.httpActionRule.table.actions', 'Actions')}</TableCell>}
          </TableRow>
        </TableHead>
        <TableBody>
          {rules
            ? rules.map((rule) => <RuleRow key={rule.id} rule={rule} onEdit={onEdit} onToggleEnabled={onToggleEnabled} />)
            : names?.map((entry) => (
                <TableRow key={entry.name} data-testid={`http-action-rule-name-row-${entry.name}`}>
                  <TableCell>{entry.name}</TableCell>
                  <TableCell>{enabledLabel(entry.enabled)}</TableCell>
                </TableRow>
              ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}
