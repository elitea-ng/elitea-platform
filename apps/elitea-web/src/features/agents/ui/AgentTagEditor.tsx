import type { ReactNode, SyntheticEvent } from 'react';
import { useCallback, useMemo } from 'react';

import Autocomplete from '@mui/material/Autocomplete';
import TextField from '@mui/material/TextField';

import type { Tag } from '@/entities/tag';
import { dedupeTagsByName, sortTagsByName, tagLabel } from '@/entities/tag';
import { useListTags } from '@/shared/api/generated/tags/tags';
import { unwrapList } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { TAG_NAME_MAX_LENGTH } from '@/shared/lib/limits';

/**
 * Old app's `NormalTagNameInputRegExp` (`common/constants.js:91`,
 * `/^[\w,\s]+$/g`, paired with `TagEditor.jsx:27`'s help text "Only
 * alphanumeric characters, white space, comma and underscore allowed")
 * collapsed into one "is this a legal tag name" check for the freeSolo-create
 * path — the character class keeps the baseline's comma allowance.
 */
function isValidTagName(name: string): boolean {
  const trimmed = name.trim();
  return trimmed.length > 0 && trimmed.length <= TAG_NAME_MAX_LENGTH && /^[\w,\s]+$/.test(trimmed);
}

/** @public */
export interface AgentTagEditorProps {
  projectId: string | undefined;
  value: readonly Tag[];
  onChange: (tags: readonly Tag[]) => void;
}

/**
 * Local, scoped-down replacement for the baseline's
 * `pages/Common/Components/TagEditor.jsx`, which wraps
 * `@/ComponentsLib/AutoCompleteDropDown` — a generic multi-select-with-
 * freeform-create component that has no port anywhere in `shared/ui` yet
 * (not part of S1's 67, not in this sub-unit's owned files). Rebuilt on
 * MUI's own `Autocomplete` (`multiple`, `freeSolo`) instead: same core
 * interaction (pick an existing tag or type a new one), without the
 * baseline's per-character colour-coding and duplicate-name inline warning
 * chrome.
 *
 * #345 — this used to be an UNEXPORTED local of `ApplicationEditForm.tsx`,
 * a file no page renders. It now has its own module and its own entry on
 * `features/agents`' public API, because the agent edit page mounts it
 * through `CreateAgentForm`'s `tagsSlot`. Moving it here did not change a
 * line of its behaviour; it only gave the page a way to reach it (R-L3
 * forbids a page importing a slice file directly).
 *
 * A tag the user types is NEW — it has no `tags` row yet, so it carries a
 * negative placeholder id purely to keep the option list keyed. The server
 * matches a tag by NAME on write (see `VersionWriteRequest.tags` in
 * services/elitea-main/api/openapi/v2.yaml), so the placeholder never
 * reaches the database and never has to be reconciled.
 */
export function AgentTagEditor({ projectId, value, onChange }: AgentTagEditorProps): ReactNode {
  // `undefined` is the tag-list QUERY parameters, not an omission: the
  // second argument became `ListTagsParams` when `entity_coverage` was
  // described. This control offers every tag the project holds, including one
  // nothing carries yet, which is what the unnarrowed list answers.
  const tagsQuery = useListTags(projectId ?? '', undefined, {
    query: { enabled: projectId !== undefined },
  });
  const availableTags = useMemo<Tag[]>(() => {
    // Unwrapped through the one helper (R-A6, #132) rather than a per-call-site
    // cast: this endpoint answers `{rows,total}` today, but the cast made that
    // assumption invisible and an unrecognised shape silently empty.
    return sortTagsByName(unwrapList<Tag>(tagsQuery.data, 'listTags'));
  }, [tagsQuery.data]);

  const handleChange = useCallback(
    (_event: SyntheticEvent, newValue: readonly (Tag | string)[]) => {
      const resolved = newValue.map((entry): Tag | undefined => {
        if (typeof entry !== 'string') return entry;
        if (!isValidTagName(entry)) return undefined;
        const existing = availableTags.find((tag) => tag.name === entry.trim());
        return existing ?? { id: -Date.now(), name: entry.trim(), data: null };
      });
      onChange(dedupeTagsByName(resolved.filter((tag): tag is Tag => tag !== undefined)));
    },
    [availableTags, onChange],
  );

  return (
    <Autocomplete
      multiple
      freeSolo
      autoSelect
      options={availableTags}
      value={value as Tag[]}
      onChange={handleChange}
      getOptionLabel={(option) => (typeof option === 'string' ? option : tagLabel(option))}
      isOptionEqualToValue={(option, selected) =>
        typeof option === 'string' || typeof selected === 'string' ? option === selected : option.id === selected.id
      }
      renderInput={(params) => (
        <TextField
          {...params}
          variant="standard"
          label={t('agents.applicationEditForm.tagsLabel', 'Tags')}
        />
      )}
    />
  );
}
