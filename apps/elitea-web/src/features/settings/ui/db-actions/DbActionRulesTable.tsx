/**
 * DbActionRulesTable — the project DB action rules list (T-5E 5e-2).
 *
 * Mirrors `HttpActionRulesTable`: callers with `models.db_action_rules.manage`
 * see full rule columns, the enabled switch, and Edit (never delete). Callers
 * with only `models.db_action_rules.list` see redacted rows (name, dialect,
 * mode, enabled) read-only — never database names, marker tables, or
 * configuration ids.
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
import type { DbActionRule, DbActionRuleName } from '@/shared/api/generated/model';
import { monoFontFamily } from '@/shared/brand/typeScale';

export interface DbActionRulesTableProps {
  readonly isLoading: boolean;
  /** Full rules (manage). When present, `names` is ignored. */
  readonly rules?: readonly DbActionRule[] | undefined;
  /** Redacted rows (list permission), rendered read-only. */
  readonly names?: readonly DbActionRuleName[] | undefined;
  readonly onEdit?: ((rule: DbActionRule) => void) | undefined;
  readonly onToggleEnabled?: ((rule: DbActionRule, enabled: boolean) => void) | undefined;
}

const tableRootSx: SxProps<Theme> = { width: '100%' };
const emptyMessageSx: SxProps<Theme> = { padding: '1.5rem', textAlign: 'center', color: 'text.secondary' };
const monoCellSx: SxProps<Theme> = {
  fontFamily: monoFontFamily,
  maxWidth: '16rem',
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

function enabledLabel(enabled: boolean): string {
  return enabled
    ? t('entities.dbActionRule.table.enabledState', 'Enabled')
    : t('entities.dbActionRule.table.disabledState', 'Disabled');
}

function dialectLabel(dialect: DbActionRule['dialect']): string {
  return dialect === 'mysql'
    ? t('entities.dbActionRule.dialect.mysql', 'MySQL')
    : t('entities.dbActionRule.dialect.postgres', 'PostgreSQL');
}

function modeLabel(mode: DbActionRule['mode']): string {
  return mode === 'write'
    ? t('entities.dbActionRule.mode.writeShort', 'Write')
    : t('entities.dbActionRule.mode.readShort', 'Read');
}

function RuleRow({ rule, onEdit, onToggleEnabled }: { rule: DbActionRule } & Pick<DbActionRulesTableProps, 'onEdit' | 'onToggleEnabled'>) {
  return (
    <TableRow data-testid={`db-action-rule-row-${rule.id}`}>
      <TableCell>{rule.name}</TableCell>
      <TableCell>{dialectLabel(rule.dialect)}</TableCell>
      <TableCell>
        <Typography variant="bodySmall" component="div" sx={monoCellSx} title={rule.database_name}>
          {rule.database_name}
        </Typography>
      </TableCell>
      <TableCell>{modeLabel(rule.mode)}</TableCell>
      <TableCell>
        <Typography variant="bodySmall" component="div" sx={monoCellSx} title={rule.marker_table || undefined}>
          {rule.marker_table || '—'}
        </Typography>
      </TableCell>
      <TableCell>{rule.configuration_id}</TableCell>
      <TableCell>
        <Switch
          checked={rule.enabled}
          onChange={(event) => onToggleEnabled?.(rule, event.target.checked)}
          slotProps={{ input: { 'aria-label': t('entities.dbActionRule.table.toggleEnabledAria', 'Toggle enabled') } }}
          data-testid={`db-action-rule-enabled-toggle-${rule.id}`}
        />
      </TableCell>
      <TableCell align="right">
        <Tooltip title={t('entities.dbActionRule.table.edit', 'Edit')}>
          <IconButton
            size="small"
            onClick={() => onEdit?.(rule)}
            aria-label={t('entities.dbActionRule.table.editAria', 'Edit DB action rule')}
            data-testid={`db-action-rule-edit-${rule.id}`}
          >
            <EditOutlinedIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      </TableCell>
    </TableRow>
  );
}

export function DbActionRulesTable({ isLoading, rules, names, onEdit, onToggleEnabled }: DbActionRulesTableProps) {
  const rowCount = rules ? rules.length : (names?.length ?? 0);
  if (!isLoading && rowCount === 0) {
    return (
      <Paper elevation={0} sx={emptyMessageSx} data-testid="db-action-rules-empty">
        <Typography variant="bodyMedium">
          {t('entities.dbActionRule.table.empty', 'No DB action rules yet. A pipeline DB node can only run against a rule defined here.')}
        </Typography>
      </Paper>
    );
  }

  return (
    <TableContainer component={Paper} elevation={0} sx={tableRootSx}>
      <Table size="small" data-testid="db-action-rules-table">
        <TableHead>
          <TableRow>
            <TableCell>{t('entities.dbActionRule.table.name', 'Name')}</TableCell>
            <TableCell>{t('entities.dbActionRule.table.dialect', 'Dialect')}</TableCell>
            {rules && <TableCell>{t('entities.dbActionRule.table.database', 'Database')}</TableCell>}
            <TableCell>{t('entities.dbActionRule.table.mode', 'Mode')}</TableCell>
            {rules && (
              <>
                <TableCell>{t('entities.dbActionRule.table.markerTable', 'Marker table')}</TableCell>
                <TableCell>{t('entities.dbActionRule.table.credential', 'Credential')}</TableCell>
              </>
            )}
            <TableCell>{t('entities.dbActionRule.table.enabled', 'Enabled')}</TableCell>
            {rules && <TableCell align="right">{t('entities.dbActionRule.table.actions', 'Actions')}</TableCell>}
          </TableRow>
        </TableHead>
        <TableBody>
          {rules
            ? rules.map((rule) => <RuleRow key={rule.id} rule={rule} onEdit={onEdit} onToggleEnabled={onToggleEnabled} />)
            : names?.map((entry) => (
                <TableRow key={entry.name} data-testid={`db-action-rule-name-row-${entry.name}`}>
                  <TableCell>{entry.name}</TableCell>
                  <TableCell>{dialectLabel(entry.dialect)}</TableCell>
                  <TableCell>{modeLabel(entry.mode)}</TableCell>
                  <TableCell>{enabledLabel(entry.enabled)}</TableCell>
                </TableRow>
              ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}
