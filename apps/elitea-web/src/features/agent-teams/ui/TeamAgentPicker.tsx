/**
 * The team editor's agent picker (#1846): a search over the project's agents.
 *
 * What the person types goes to the list route's `query` (debounced), so any
 * agent in the project can be found however many there are; the list shows
 * the first page of matches, and says so when there are more to narrow.
 */
import type { ReactNode, SyntheticEvent } from 'react';
import { useEffect, useState } from 'react';

import Autocomplete from '@mui/material/Autocomplete';
import TextField from '@mui/material/TextField';

import { useListApplications } from '@/shared/api/generated/applications/applications';
import type { Application } from '@/shared/api/generated/model';
import { unwrapListPage } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

import { hasMember, type TeamDraft } from '../lib/teamDraft';

/** The matches one search shows. */
const PICKER_PAGE_SIZE = 50;
const SEARCH_DELAY_MS = 250;

function useSettled(value: string): string {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), SEARCH_DELAY_MS);
    return () => clearTimeout(timer);
  }, [value]);
  return settled;
}

export function TeamAgentPicker({
  projectId,
  draft,
  disabled,
  onPick,
}: {
  readonly projectId: string;
  readonly draft: TeamDraft;
  readonly disabled: boolean;
  readonly onPick: (agent: Application) => void;
}): ReactNode {
  const [input, setInput] = useState('');
  const search = useSettled(input.trim());
  const params = { agents_type: 'classic', limit: PICKER_PAGE_SIZE, ...(search === '' ? {} : { query: search }) };
  const list = useListApplications(projectId, params, { query: { enabled: projectId !== '' } });
  const { rows: matches, total } = unwrapListPage<Application>(list.data, 'listApplications(team picker)');
  const agents = matches.filter((agent) => !hasMember(draft, Number(agent.id), Number(agent.project_id ?? projectId)));
  const more = total > matches.length;

  const pick = (_event: SyntheticEvent, agent: Application | null): void => {
    if (agent === null) return;
    setInput('');
    onPick(agent);
  };

  return (
    <Autocomplete<Application>
      options={agents}
      value={null}
      inputValue={input}
      onInputChange={(_event, value, reason) => {
        if (reason !== 'reset') setInput(value);
      }}
      onChange={pick}
      disabled={disabled}
      loading={list.isFetching}
      filterOptions={(options) => options}
      getOptionLabel={(agent) => agent.name}
      isOptionEqualToValue={(option, value) => option.id === value.id}
      noOptionsText={t('features.agentTeams.edit.noAgents', 'No agents match.')}
      renderOption={({ key, ...props }, agent) => (
        <li key={key} {...props} data-testid={`agent-team-edit-add-${agent.id}`}>
          {agent.name}
        </li>
      )}
      renderInput={(inputParams) => (
        <TextField
          {...inputParams}
          label={t('features.agentTeams.edit.addAgent', 'Add an agent')}
          placeholder={t('features.agentTeams.edit.searchAgents', 'Search the project\'s agents')}
          helperText={more ? t('features.agentTeams.edit.moreAgents', 'Showing {{shown}} of {{total}} agents. Type to narrow the list.', { shown: matches.length, total }) : undefined}
          slotProps={{
            ...inputParams.slotProps,
            htmlInput: { ...inputParams.slotProps.htmlInput, 'data-testid': 'agent-team-edit-add' },
          }}
        />
      )}
    />
  );
}
