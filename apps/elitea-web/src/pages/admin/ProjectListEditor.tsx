/**
 * A1 (ELITEA-0016, admin-portal/agent-publishing-guardrails): the project
 * PICKER behind `publish_whitelist_project_ids`/`skill_publish_whitelist_project_ids`
 * — a name-searchable, Team/Private/Public-filterable multi-select, replacing
 * `ConfigurationListEditor`'s raw integer-id rows for these two fields only
 * (still storing plain project ids; see `configurationFields.ts`'s
 * `widgetFor`, which routes only fields whose key ends `whitelist_project_ids`
 * here).
 *
 * Same `useAdminProjects` org-wide listing and `Autocomplete`
 * multiple-multi-select shape `PlatformModelGrantFields.tsx`'s
 * `GrantedProjectsPicker` already established for "the project picker
 * Bulk invite uses" — deliberately reused rather than re-invented, for the
 * same reason that file's own doc comment gives: a second picker control is a
 * second set of behaviours an operator has to learn.
 *
 * **Public, beyond `useAdminProjects`'s own `team`/`personal` split.** The
 * server's `project_type` filter only distinguishes team/personal
 * (`ProjectType` — no `public` value exists there; the public project is an
 * ordinary team-typed row at a well-known id). This control adds that third
 * bucket client-side: `isPublicProject(row.id, publicProjectId)` checked
 * BEFORE `row.is_personal`, using the same `VITE_PUBLIC_PROJECT_ID` config
 * value `AgentEditorPanel.derive.ts`'s `resolvePublicProjectId` reads
 * (reproduced locally here rather than imported — that function lives in
 * `features/chat-input`, not a shared/entities home, and duplicating a
 * two-line config read is cheaper than promoting it for one more caller).
 */
import type { ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import Autocomplete from '@mui/material/Autocomplete';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';

import { isPublicProject } from '@/entities/project';
import { getConfig } from '@/shared/config';
import { t } from '@/shared/i18n';

import { useAdminProjects, type AdminProjectRow } from './api/adminProjectsApi';

/** Same 2-line read `AgentEditorPanel.derive.ts`'s `resolvePublicProjectId` uses — see module doc for why it is not imported from there instead. */
function resolvePublicProjectId(): string {
  const result = getConfig();
  return result.status === 'ok' ? result.config.vite_public_project_id : '';
}

/** The three-way filter this control adds on top of `useAdminProjects`'s own team/personal split. */
export type ProjectListFilter = 'all' | 'team' | 'private' | 'public';

/** Classifies one row — `public` (the well-known project) takes priority over `is_personal`, since nothing stops the public project's row from also being named like a personal one. */
export function classifyProjectRow(row: AdminProjectRow, publicProjectId: string): Exclude<ProjectListFilter, 'all'> {
  if (isPublicProject(row.id, publicProjectId)) return 'public';
  return row.is_personal ? 'private' : 'team';
}

/** A selected id the current page did not return — shown by id rather than silently dropped; same precedent `PlatformModelGrantFields.tsx`'s own `placeholderProject` sets. */
function placeholderProjectRow(id: number): AdminProjectRow {
  return {
    id,
    name: t('pages.admin.configuration.projectListEditor.projectById', 'Project {{id}}', { id }),
    owner_id: 0,
    owner_name: '',
    admin_names: [],
    status: 'active',
    suspended: false,
    create_success: true,
    is_personal: false,
  };
}

const PICKER_PAGE_SIZE = 50;

export interface ProjectListEditorProps {
  readonly fieldKey: string;
  readonly label: string;
  readonly value: readonly number[];
  readonly disabled?: boolean | undefined;
  readonly onChange: (next: readonly number[]) => void;
}

export function ProjectListEditor({ fieldKey, label, value, disabled = false, onChange }: ProjectListEditorProps): ReactNode {
  const [search, setSearch] = useState('');
  const [filter, setFilter] = useState<ProjectListFilter>('all');
  const publicProjectId = resolvePublicProjectId();

  const projectsQuery = useAdminProjects({ limit: PICKER_PAGE_SIZE, offset: 0, search: search === '' ? undefined : search });
  const allRows = useMemo(() => projectsQuery.data?.rows ?? [], [projectsQuery.data]);

  const classify = useCallback((row: AdminProjectRow) => classifyProjectRow(row, publicProjectId), [publicProjectId]);
  const options = useMemo(
    () => (filter === 'all' ? allRows : allRows.filter((row) => classify(row) === filter)),
    [allRows, filter, classify],
  );

  // Looked up against the UNFILTERED page, so a selected project the current
  // type filter hides (or the current search text does not match) still
  // shows its real name rather than a placeholder — the filter/search narrow
  // what can be ADDED, not what is already chosen.
  const selected = useMemo(
    () => value.map((id) => allRows.find((row) => row.id === id) ?? placeholderProjectRow(id)),
    [value, allRows],
  );

  return (
    <Box
      sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}
      data-testid={`admin-config-list-${fieldKey}`}
    >
      <TextField
        select
        size="small"
        label={t('pages.admin.configuration.projectListEditor.filterLabel', 'Show')}
        value={filter}
        disabled={disabled}
        onChange={(event) => { setFilter(event.target.value as ProjectListFilter); }}
        slotProps={{ htmlInput: { 'data-testid': `${fieldKey}-type-filter` } }}
        sx={{ maxWidth: '12rem' }}
      >
        <MenuItem value="all">{t('pages.admin.configuration.projectListEditor.filterAll', 'All types')}</MenuItem>
        <MenuItem value="team">{t('pages.admin.configuration.projectListEditor.filterTeam', 'Team')}</MenuItem>
        <MenuItem value="private">{t('pages.admin.configuration.projectListEditor.filterPrivate', 'Private')}</MenuItem>
        <MenuItem value="public">{t('pages.admin.configuration.projectListEditor.filterPublic', 'Public')}</MenuItem>
      </TextField>

      <Autocomplete
        multiple
        disableCloseOnSelect
        disabled={disabled}
        options={options}
        value={selected}
        getOptionLabel={(project: AdminProjectRow) => project.name}
        isOptionEqualToValue={(option, val) => option.id === val.id}
        loading={projectsQuery.isFetching}
        onInputChange={(_event, next) => { setSearch(next); }}
        onChange={(_event, next) => { onChange(next.map((project) => project.id)); }}
        renderValue={(values, getItemProps) =>
          values.map((project, index) => (
            <Chip
              {...getItemProps({ index })}
              key={project.id}
              size="small"
              label={project.name}
            />
          ))
        }
        renderInput={(params) => (
          <TextField
            {...params}
            size="small"
            label={label}
            placeholder={t('pages.admin.configuration.projectListEditor.searchPlaceholder', 'Search by project name')}
            data-testid={`${fieldKey}-project-picker`}
          />
        )}
      />
    </Box>
  );
}
