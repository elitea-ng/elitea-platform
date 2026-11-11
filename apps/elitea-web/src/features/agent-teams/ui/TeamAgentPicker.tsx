/**
 * The team editor's agent picker (#1846): a search over the project's agents.
 *
 * What the person types goes to the list route's `query` (debounced), so any
 * agent in the project can be found however many there are; the list shows
 * the first page of matches, and says so when there are more to narrow.
 */
import type { ReactNode, SyntheticEvent } from 'react';
import { useState } from 'react';

import Autocomplete from '@mui/material/Autocomplete';
import TextField from '@mui/material/TextField';

import { useListApplications } from '@/shared/api/generated/applications/applications';
import type { Application } from '@/shared/api/generated/model';
import { unwrapListPage } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { useDebounce } from '@/shared/lib/useDebounce';

import { MAX_TEAM_MEMBERS, hasMember, type TeamDraft } from '../lib/teamDraft';

/**
 * The matches one search shows. The page asks for room for every member
 * too, so leaving the team's own agents out still leaves this many.
 */
const PICKER_PAGE_SIZE = 50;
const SEARCH_DELAY_MS = 250;

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
  const search = useDebounce(input.trim(), SEARCH_DELAY_MS);
  const params = { agents_type: 'classic', limit: PICKER_PAGE_SIZE + MAX_TEAM_MEMBERS, ...(search === '' ? {} : { query: search }) };
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
      loadingText={t('features.agentTeams.edit.loadingAgents', 'Loading agents…')}
      renderOption={({ key: _byName, ...props }, agent) => (
        // MUI keys an option by its label; two agents may share a name.
        <li key={agent.id} {...props} data-testid={`agent-team-edit-add-${agent.id}`}>
          {agent.name}
        </li>
      )}
      renderInput={(inputParams) => (
        <TextField
          {...inputParams}
          label={t('features.agentTeams.edit.addAgent', 'Add an agent')}
          placeholder={t('features.agentTeams.edit.searchAgents', 'Search the project\'s agents')}
          helperText={more ? t('features.agentTeams.edit.moreAgents', 'More agents match. Type to narrow the list.') : undefined}
          slotProps={{
            ...inputParams.slotProps,
            htmlInput: { ...inputParams.slotProps.htmlInput, 'data-testid': 'agent-team-edit-add' },
          }}
        />
      )}
    />
  );
}
