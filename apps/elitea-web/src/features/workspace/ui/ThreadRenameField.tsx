/**
 * The inline field that renames a local thread (the sidebar's Rename, the
 * toolbar's ⋯ ▸ Rename Thread): Enter renames the conversation on the server
 * and then the thread here, and closes the field once the server took it; a
 * refused rename keeps the field open and says so. Escape, or leaving the
 * field, keeps the name.
 */
import { useState, type KeyboardEvent } from 'react';

import Box from '@mui/material/Box';
import InputBase from '@mui/material/InputBase';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

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
    if (next === '' || next === name) {
      onDone();
      return;
    }
    rename.mutate({ ref: thread, name: next }, { onSuccess: onDone });
  };
  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    if (event.key === 'Enter') submit();
    else if (event.key === 'Escape') onDone();
  };
  const failed = rename.isError;
  return (
    <Box
      sx={{
        display: 'flex',
        flexDirection: 'column',
        flex: 1,
        minWidth: 0,
        gap: 0.25,
      }}
    >
      <InputBase
        value={value}
        onChange={(event) => setValue(event.target.value.slice(0, MAX_NAME))}
        onKeyDown={onKeyDown}
        onBlur={() => {
          // A rename on its way, or one refused, keeps the field.
          if (!rename.isPending && !failed) onDone();
        }}
        disabled={rename.isPending}
        error={failed}
        onClick={(event) => event.stopPropagation()}
        inputRef={(node: HTMLInputElement | null) => node?.focus()}
        inputProps={{
          'aria-label': t('workspace.thread.renameLabel', 'Thread name'),
          'aria-invalid': failed,
          'data-testid': 'thread-rename',
        }}
        sx={(theme: Theme) => ({
          flex: 1,
          minWidth: 0,
          paddingX: 0.5,
          borderRadius: theme.vars.shape.radiusSm,
          border: `1px solid ${failed ? theme.vars.palette.error.main : theme.vars.palette.primary.main}`,
          background: theme.vars.palette.background.default,
          typography: 'labelMedium',
        })}
      />
      {failed && (
        <Typography role="alert" variant="bodySmall" data-testid="thread-rename-error" sx={(theme: Theme) => ({ color: theme.vars.palette.error.main })}>
          {t('workspace.thread.renameFailed', 'The thread could not be renamed. Try again, or press Esc.')}
        </Typography>
      )}
    </Box>
  );
}
