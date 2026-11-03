/**
 * The composer footer's row (`ComposerRow`), Send excepted (the input draws it
 * at the row's end):
 *
 * - leading, which shrinks: attach images (+), the mode ("Act ⌄",
 *   `ModePicker`), the agent·version picker, the model, then ⋯ for what is
 *   used less (the step limit);
 * - trailing, which keeps its room: where the thread runs ("Local ⌄"), the
 *   meters (the language servers and the context ring) and "Run to
 *   completion" (∞), just before Send.
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
  runToCompletion: ReactNode;
}

function FooterControls({ showMode, attach, agentPicker, modelPicker, more, runLocation, contextMeter, runToCompletion, ...mode }: FooterControlsProps): React.JSX.Element {
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
          {runToCompletion}
        </>
      }
    />
  );
}

/** The composer props the footer reads (`WorkspaceComposerProps`). */
export interface FooterSource {
  planMode: boolean;
  onPlanModeChange: (planMode: boolean) => void;
  bypass?: boolean;
  onBypassChange?: ((on: boolean) => void) | undefined;
  showMode?: boolean;
  agentPicker?: ReactNode;
  modelPicker?: ReactNode;
  more?: ((close: () => void) => ReactNode) | undefined;
  runLocation?: ReactNode;
  contextMeter?: ReactNode;
  runToCompletion?: ReactNode;
}

/** The footer row of the composer whose props are `source`, with `attach` (the + that picks images, or none). */
export function composerFooter(source: FooterSource, attach: ReactNode): React.JSX.Element {
  return (
    <FooterControls
      planMode={source.planMode}
      onPlanModeChange={source.onPlanModeChange}
      bypass={source.bypass === true}
      onBypassChange={source.onBypassChange}
      showMode={source.showMode !== false}
      attach={attach}
      agentPicker={source.agentPicker}
      modelPicker={source.modelPicker}
      more={source.more}
      runLocation={source.runLocation}
      contextMeter={source.contextMeter}
      runToCompletion={source.runToCompletion}
    />
  );
}
