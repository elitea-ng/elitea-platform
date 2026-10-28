/**
 * The composer footer's row (`ComposerRow`), Send excepted (the input draws it
 * at the row's end):
 *
 * - leading, which shrinks: attach images (+), the mode ("Act ⌄",
 *   `ModePicker`), the agent·version picker, the model, then ⋯ for what is
 *   used less (the step limit);
 * - trailing, which keeps its room: where the thread runs ("Local ⌄") and the
 *   meters (the language servers and the context ring).
 */
import type { ReactNode } from 'react';

import { ComposerMoreMenu, ComposerRow } from '@/shared/ui/native-composer';

import { ModePicker, type ModePickerProps } from './ModePicker';

interface FooterControlsProps extends ModePickerProps {
  showMode: boolean;
  /** The attach-images button; none when the thread takes no images. */
  attach: ReactNode;
  agentPicker: ReactNode;
  modelPicker: ReactNode;
  /** The ⋯ menu's rows; no ⋯ without them. */
  more: ((close: () => void) => ReactNode) | undefined;
  runLocation: ReactNode;
  contextMeter: ReactNode;
}

export function FooterControls({ showMode, attach, agentPicker, modelPicker, more, runLocation, contextMeter, ...mode }: FooterControlsProps): React.JSX.Element {
  return (
    <ComposerRow
      leading={
        <>
          {attach}
          {showMode && <ModePicker {...mode} />}
          {agentPicker}
          {modelPicker}
          {more !== undefined && <ComposerMoreMenu testId="workspace-composer-more">{more}</ComposerMoreMenu>}
        </>
      }
      trailing={
        <>
          {runLocation}
          {contextMeter}
        </>
      }
    />
  );
}
