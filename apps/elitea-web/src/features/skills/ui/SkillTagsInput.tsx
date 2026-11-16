import type { ReactNode } from 'react';
import { useState } from 'react';

import Autocomplete from '@mui/material/Autocomplete';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';
import { TAG_NAME_MAX_LENGTH } from '@/shared/lib/limits';

export interface SkillTagsInputProps {
  readonly value: readonly string[];
  readonly onChange: (tags: readonly string[]) => void;
  readonly disabled?: boolean;
}

interface NormalisedTags {
  readonly tags: readonly string[];
  /** True when a tag over the length cap was refused (never truncated: a cut tag can collide with another). */
  readonly refusedTooLong: boolean;
}

/** Trimmed, non-empty, within the tag length cap, and unique ignoring case. */
function normaliseTags(tags: readonly string[]): NormalisedTags {
  const seen = new Set<string>();
  const result: string[] = [];
  let refusedTooLong = false;
  for (const raw of tags) {
    const tag = raw.trim();
    if (tag.length > TAG_NAME_MAX_LENGTH) {
      refusedTooLong = true;
      continue;
    }
    const key = tag.toLowerCase();
    if (tag === '' || seen.has(key)) continue;
    seen.add(key);
    result.push(tag);
  }
  return { tags: result, refusedTooLong };
}

/**
 * The skill's tag field: chips, with Enter and comma both ending the tag being
 * typed (#2101).
 *
 * The control it replaces held the tags as one comma-joined string and split it
 * on every keystroke, so the comma was parsed away the moment it was typed and
 * Enter had no handler: no tag could be added, only existing ones edited.
 */
export function SkillTagsInput({ value, onChange, disabled = false }: SkillTagsInputProps): ReactNode {
  const [input, setInput] = useState('');
  const [tooLong, setTooLong] = useState(false);

  const commit = (candidates: readonly string[]): void => {
    const { tags, refusedTooLong } = normaliseTags(candidates);
    setTooLong(refusedTooLong);
    onChange(tags);
  };

  const handleInput = (next: string): void => {
    if (!next.includes(',')) {
      setInput(next);
      return;
    }
    // A comma ends the tag(s) before it; whatever follows the last comma stays in the box.
    const parts = next.split(',');
    const rest = parts.pop() ?? '';
    commit([...value, ...parts]);
    setInput(rest);
  };

  return (
    <Autocomplete<string, true, false, true>
      multiple
      freeSolo
      autoSelect
      disabled={disabled}
      options={[]}
      value={[...value]}
      inputValue={input}
      onInputChange={(_event, next, reason) => {
        if (reason === 'input') handleInput(next);
        else setInput(next);
      }}
      onChange={(_event, next) => commit(next)}
      renderInput={(params) => (
        <TextField
          {...params}
          label={t('skills.form.tags', 'Tags')}
          error={tooLong}
          helperText={
            tooLong
              ? t('skills.form.tagTooLong', 'A tag can be at most {{max}} characters. That tag was not added.', { max: TAG_NAME_MAX_LENGTH })
              : t('skills.form.tagsHint', 'Press Enter or type a comma to add a tag.')
          }
          slotProps={{
            ...params.slotProps,
            htmlInput: { ...params.slotProps.htmlInput, 'data-testid': 'skill-tags-input' },
          }}
        />
      )}
    />
  );
}
