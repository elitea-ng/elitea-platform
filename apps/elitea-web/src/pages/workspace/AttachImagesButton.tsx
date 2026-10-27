/**
 * The composer footer's +: pick images from a file dialog. The picked files
 * go the way a pasted image does (`useImageAttachments`' `attachPicked`: the
 * host checks, shrinks and holds them); paste and drop keep working beside it.
 */
import { useRef } from 'react';

import AddIcon from '@mui/icons-material/Add';

import { t } from '@/shared/i18n';
import { ComposerIconButton } from '@/shared/ui/native-composer';

export function AttachImagesButton({ onPick }: { onPick: (files: readonly File[]) => void }): React.JSX.Element {
  const input = useRef<HTMLInputElement>(null);
  return (
    <>
      <ComposerIconButton label={t('workspace.composer.attachImages', 'Attach images')} data-testid="workspace-attach-images" onClick={() => input.current?.click()}>
        <AddIcon />
      </ComposerIconButton>
      <input
        ref={input}
        type="file"
        accept="image/*"
        multiple
        hidden
        data-testid="workspace-attach-images-input"
        onChange={(event) => {
          const files = Array.from(event.target.files ?? []);
          // The same file can be picked again after it was removed.
          event.target.value = '';
          if (files.length > 0) onPick(files);
        }}
      />
    </>
  );
}
