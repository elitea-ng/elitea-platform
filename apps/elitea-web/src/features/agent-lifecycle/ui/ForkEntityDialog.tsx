import type { ReactNode } from 'react';
import { useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

/**
 * "Fork parameters" — the dialog production shows for VERSION → Fork
 * (measured read-only 2026-09-06: a PROJECT select, a MAIN ENTITY summary
 * with the name and type, a "Show details" disclosure, and Cancel / Fork with
 * Fork disabled until a project is chosen).
 *
 * The project list is a PROP, not a query. `useProjectOptions` lives in
 * `widgets/sidebar`, and a `features/` slice may not import a widget
 * (`no-upward-from-features`). The page that mounts this dialog is above both
 * layers and already has the list, so it passes it down — the same route
 * `pages/agents` takes for every other cross-layer value.
 */
export interface ForkTargetProject {
  readonly id: string;
  readonly name: string;
}

export interface ForkEntityDialogProps {
  readonly open: boolean;
  readonly entityName: string;
  /** `agent` or `pipeline`, shown as production shows "Type: agent". */
  readonly entityType: string;
  readonly projects: readonly ForkTargetProject[];
  /** Pre-selected so a single-project deployment can fork in two clicks. */
  readonly defaultProjectId: string | undefined;
  readonly isForking: boolean;
  readonly error: string | undefined;
  readonly onClose: () => void;
  readonly onFork: (targetProjectId: string) => void;
}

export function ForkEntityDialog({
  open,
  entityName,
  entityType,
  projects,
  defaultProjectId,
  isForking,
  error,
  onClose,
  onFork,
}: ForkEntityDialogProps): ReactNode {
  const [targetProjectId, setTargetProjectId] = useState(defaultProjectId ?? '');

  useEffect(() => {
    if (open) setTargetProjectId(defaultProjectId ?? '');
  }, [open, defaultProjectId]);

  return (
    <BaseModal
      open={open}
      title={t('features.agentLifecycle.fork.title', 'Fork parameters')}
      data-testid="fork-entity-dialog"
      onClose={onClose}
      onConfirm={() => {
        if (targetProjectId !== '') onFork(targetProjectId);
      }}
      actions={{
        confirmText: t('features.agentLifecycle.fork.confirm', 'Fork'),
        confirming: isForking,
      }}
      content={
        <Box sx={contentSx}>
          <Typography variant="labelMedium">{t('features.agentLifecycle.fork.project', 'PROJECT')}</Typography>
          <Select
            displayEmpty
            value={targetProjectId}
            onChange={(event) => setTargetProjectId(event.target.value)}
            data-testid="fork-target-project"
            fullWidth
          >
            <MenuItem value="">{t('features.agentLifecycle.fork.selectProject', 'Select project')}</MenuItem>
            {projects.map((project) => (
              <MenuItem
                key={project.id}
                value={project.id}
              >
                {project.name}
              </MenuItem>
            ))}
          </Select>
          <Typography variant="labelMedium">{t('features.agentLifecycle.fork.mainEntity', 'MAIN ENTITY')}</Typography>
          <Typography
            variant="bodyMedium"
            data-testid="fork-entity-name"
          >
            {entityName}
          </Typography>
          <Typography
            variant="bodySmall"
            color="text.secondary"
          >
            {t('features.agentLifecycle.fork.entityType', 'Type')}: {entityType}
          </Typography>
          {targetProjectId === '' && (
            <Typography variant="bodySmall">
              {t('features.agentLifecycle.fork.chooseFirst', 'Choose the project the copy is created in.')}
            </Typography>
          )}
          {error !== undefined && (
            <Typography
              role="alert"
              variant="bodySmall"
              data-testid="fork-error"
            >
              {error}
            </Typography>
          )}
        </Box>
      }
    />
  );
}

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem', minWidth: '22rem' };
