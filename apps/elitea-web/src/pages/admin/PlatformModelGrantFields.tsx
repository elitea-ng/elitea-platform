/**
 * "Available to" — which projects a platform model is offered to.
 *
 * ## Why this is a control and not a flag
 *
 * Publishing a platform model used to be one decision with one outcome: every
 * project on the deployment got it. An operator who wanted a model for two
 * teams had to copy the row into each of their projects, which makes two models
 * that then drift. The three choices here are that decision, made once, on the
 * row the whole platform reads.
 *
 * ## The project picker is the one Bulk invite uses
 *
 * Same `useAdminProjects` listing, same server-side `search`, same
 * multi-select. It is deliberately the same control: an operator who has
 * granted projects anywhere else in this panel has met it, and a second way to
 * choose a project would be a second set of behaviours to learn — and a second
 * place for the `team`/`personal` split to be got wrong.
 *
 * Only TEAM projects are offered. A personal project is one person's own
 * workspace; granting a platform model to it is a grant to that person, which
 * is a decision this screen does not make and could not report back (the list
 * is thousands of rows long on a real deployment).
 *
 * ## The chosen projects survive a search that no longer finds them
 *
 * The options come from ONE page of the server's listing, narrowed by whatever
 * is typed. The value is held by the form, not by the list, so a project chosen
 * before the search was typed stays chosen — a picker that intersected the two
 * would silently drop grants as the operator looked for the next project.
 */
import type { ReactNode } from 'react';
import { useMemo, useState } from 'react';

import Autocomplete from '@mui/material/Autocomplete';
import Chip from '@mui/material/Chip';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';

import { useAdminProjects, type AdminProjectRow } from './api/adminProjectsApi';
import type { ModelForm } from './platformModelForm';
import { shareScopeLabel, shareScopeOptions, type ShareScope } from './platformModelGrant';

/** One page of the picker, matching the size Bulk invite asks for. */
const PICKER_PAGE_SIZE = 50;

export interface PlatformModelGrantFieldsProps {
  readonly form: ModelForm;
  readonly onChange: <K extends keyof ModelForm>(key: K, value: ModelForm[K]) => void;
}

export function PlatformModelGrantFields({
  form,
  onChange,
}: PlatformModelGrantFieldsProps): ReactNode {
  return (
    <>
      <TextField
        select
        label={t('pages.admin.platformModels.field.shareScope', 'Available to')}
        value={form.shareScope}
        onChange={(event) => {
          onChange('shareScope', event.target.value as ShareScope);
        }}
        size="small"
        slotProps={{ htmlInput: { 'data-testid': 'platform-model-share-scope' } }}
        helperText={t(
          'pages.admin.platformModels.field.shareScopeHelp',
          'Which projects may see and use this model. A project with no grant cannot select it and cannot call it.',
        )}
      >
        {shareScopeOptions().map((scope) => (
          <MenuItem key={scope} value={scope}>
            {shareScopeLabel(scope)}
          </MenuItem>
        ))}
      </TextField>

      {/* The picker is a CHILD, mounted only for the scope that reads it. The
          project listing is a server read, and a hook at this level would fetch
          a page of projects for every operator who opened this dialog to rename
          a model. */}
      {form.shareScope === 'projects' ? (
        <GrantedProjectsPicker form={form} onChange={onChange} />
      ) : null}
    </>
  );
}

/** The multi-select, and the one query that fills it. */
function GrantedProjectsPicker({ form, onChange }: PlatformModelGrantFieldsProps): ReactNode {
  const [search, setSearch] = useState('');
  const projectsQuery = useAdminProjects({
    limit: PICKER_PAGE_SIZE,
    offset: 0,
    search: search === '' ? undefined : search,
    projectType: 'team',
  });

  /* `useMemo` keeps the option identity stable across the re-renders a
     selection causes; a fresh array each render makes MUI re-open the list on
     every keystroke — the same reason Bulk invite memoises its own. */
  const options = useMemo(() => projectsQuery.data?.rows ?? [], [projectsQuery.data]);

  /* The VALUE is built from the ids the form holds, so a granted project the
     current search does not return is still shown — as its id when its row is
     not in the page, which is honest about what is known rather than dropping
     the grant. */
  const selected = useMemo(
    () =>
      form.sharedWith.map(
        (id) => options.find((project) => project.id === id) ?? placeholderProject(id),
      ),
    [form.sharedWith, options],
  );

  return (
    <Autocomplete
      multiple
      disableCloseOnSelect
      options={options}
      value={selected}
      getOptionLabel={(project: AdminProjectRow) => project.name}
      isOptionEqualToValue={(option, value) => option.id === value.id}
      loading={projectsQuery.isFetching}
      onInputChange={(_event, value) => setSearch(value)}
      onChange={(_event, value) => {
        onChange(
          'sharedWith',
          value.map((project) => project.id),
        );
      }}
      renderValue={(values, getItemProps) =>
        values.map((project, index) => (
          <Chip {...getItemProps({ index })} key={project.id} size="small" label={project.name} />
        ))
      }
      renderInput={(params) => (
        <TextField
          {...params}
          size="small"
          required
          label={t('pages.admin.platformModels.field.sharedWith', 'Projects')}
          placeholder={t(
            'pages.admin.platformModels.field.sharedWithPlaceholder',
            'Search by project name',
          )}
          data-testid="platform-model-shared-with"
          helperText={t(
            'pages.admin.platformModels.field.sharedWithHelp',
            'At least one. A model available to no selected project is available to nobody.',
          )}
        />
      )}
    />
  );
}

/**
 * A granted project the current page of the listing does not carry.
 *
 * It is shown by its id rather than dropped. The alternative was measured on
 * the same control elsewhere: an option list that decided the value silently
 * removes every grant the operator cannot currently see.
 */
function placeholderProject(id: number): AdminProjectRow {
  return {
    id,
    name: t('pages.admin.platformModels.field.projectById', 'Project {{id}}', { id }),
    owner_id: 0,
    owner_name: '',
    admin_names: [],
    status: 'active',
    suspended: false,
    create_success: true,
    is_personal: false,
  };
}
