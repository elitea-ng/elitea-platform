/**
 * What the toolkit editor tells the person about a Save they pressed (#2016).
 *
 * Save used to answer a click with nothing at all in three cases: a form that
 * still had invalid fields (it only asked the form to validate, which paints
 * fields that may be scrolled away in a narrow editor such as the chat side
 * panel), a refusal the server did not key to a field (the page's banner does
 * not exist in the chat editor), and a toolkit without an id. Each is now said
 * in words, in a snackbar over the editor; a successful save is confirmed too.
 */
import { useCallback, useMemo, useState } from 'react';
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Snackbar from '@mui/material/Snackbar';

import { EliteaApiError } from '@/shared/api/generated/mutator';
import { serverErrorSentence } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';

interface Notice {
  readonly severity: 'success' | 'error';
  readonly message: string;
}

export interface ToolkitEditorNotice {
  readonly saved: () => void;
  readonly blocked: () => void;
  /** `keyed`: the refusal was painted on form fields already. */
  readonly failed: (error: unknown, keyed: boolean) => void;
  readonly view: ReactNode;
}

export function useToolkitEditorNotice(): ToolkitEditorNotice {
  const [notice, setNotice] = useState<Notice | undefined>(undefined);
  const dismiss = useCallback(() => setNotice(undefined), []);
  const saved = useCallback(() => setNotice({ severity: 'success', message: t('toolkits.editor.saved', 'Toolkit saved.') }), []);
  const blocked = useCallback(
    () => setNotice({ severity: 'error', message: t('toolkits.editor.blocked', 'The toolkit was not saved: some fields are not valid. Fix the highlighted fields and try again.') }),
    [],
  );
  const failed = useCallback((error: unknown, keyed: boolean) => {
    const explained = error instanceof EliteaApiError && error.failure.kind === 'http' ? serverErrorSentence(error.failure.body) : undefined;
    const fallback = keyed
      ? t('toolkits.editor.refusedFields', 'The toolkit was not saved: some settings were refused. Check the highlighted fields.')
      : t('toolkits.editor.saveFailed', 'The toolkit could not be saved. Try again.');
    setNotice({ severity: 'error', message: keyed ? fallback : (explained ?? fallback) });
  }, []);
  const view = useMemo(
    () => (
      <Snackbar open={notice !== undefined} autoHideDuration={6000} onClose={dismiss} anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}>
        <Alert severity={notice?.severity ?? 'error'} variant="filled" onClose={dismiss}>
          {notice?.message}
        </Alert>
      </Snackbar>
    ),
    [notice, dismiss],
  );
  return { saved, blocked, failed, view };
}
