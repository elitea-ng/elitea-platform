/**
 * The inline field that renames a local thread (the sidebar's Rename, the
 * toolbar's ⋯ ▸ Rename Thread): Enter renames the conversation on the server
 * and then the thread here; Escape, or leaving the field, keeps the name.
 */
import { useState, type KeyboardEvent } from 'react';

import InputBase from '@mui/material/InputBase';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { useRenameThread, type ThreadRef } from '../model/threadNames';

/** The longest name the server stores on a rename. */
const MAX_NAME = 256;

export interface ThreadRenameFieldProps {
  thread: ThreadRef;
  name: string;
  onDone: () => void;
}

export function ThreadRenameField({ thread, name, onDone }: ThreadRenameFieldProps): React.JSX.Element {
  const [value, setValue] = useState(name);
  const rename = useRenameThread();
  const submit = (): void => {
    const next = value.trim();
    if (next !== '' && next !== name) rename.mutate({ ref: thread, name: next });
    onDone();
  };
  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    if (event.key === 'Enter') submit();
    else if (event.key === 'Escape') onDone();
  };
  return (
    <InputBase
      value={value}
      onChange={(event) => setValue(event.target.value.slice(0, MAX_NAME))}
      onKeyDown={onKeyDown}
      onBlur={onDone}
      onClick={(event) => event.stopPropagation()}
      inputRef={(node: HTMLInputElement | null) => node?.focus()}
      inputProps={{ 'aria-label': t('workspace.thread.renameLabel', 'Thread name'), 'data-testid': 'thread-rename' }}
      sx={(theme: Theme) => ({
        flex: 1,
        minWidth: 0,
        paddingX: 0.5,
        borderRadius: theme.vars.shape.radiusSm,
        border: `1px solid ${theme.vars.palette.primary.main}`,
        background: theme.vars.palette.background.default,
        typography: 'labelMedium',
      })}
    />
  );
}
