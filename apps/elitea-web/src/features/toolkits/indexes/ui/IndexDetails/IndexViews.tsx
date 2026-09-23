import type { ComponentType, RefObject, ReactNode } from 'react';
import { useMemo } from 'react';

import { EditViewTabsEnum } from '../../lib/constants/indexDetails.constants';
import type { JsonSchemaLike } from '../../lib/helpers/indexChat.helpers';
import type { IndexRow } from '../../model/indexesStore';

import type { IndexConfigToolsConfig, ToolFormFieldProps } from './IndexConfig';
import { IndexConfig } from './IndexConfig';
import type { IndexHistoryItem } from './IndexHistory';
import { IndexHistory } from './IndexHistory';

/**
 * Port of `apps/elitea-ui/src/[fsd]/features/toolkits/indexes/ui/
 * IndexDetails/IndexViews.jsx` (unit A4a) — switches between the
 * Run/Configuration/History tab bodies.
 *
 * THE CONFIGURATION TAB IS EDITABLE NOW (issue 940/A5). It used to pass a
 * hardcoded `changesDisabled` — every field on it was read-only — and that
 * was the correct state of affairs while there was nothing to save it with:
 * the only writer of an index's stored `index_configuration` was an indexing
 * RUN, so an editable field would have offered a change no button could
 * persist, and a "Reindex" pressed afterwards would have run edits the stored
 * configuration never received.
 *
 * With the Save / Save & Reindex pair (`IndexActionsParts.tsx`) there is a
 * writer, and the read-only flag is what stands between a person and the
 * feature. The hazard it was guarding against is handled where it belongs
 * instead: "Reindex" is WITHDRAWN while the form is dirty, so a run can only
 * ever be started from a configuration the server holds.
 */
export interface IndexViewsProps {
  readonly activeView: string;
  readonly schema: JsonSchemaLike | null | undefined;
  readonly toolsConfig?: IndexConfigToolsConfig | null | undefined;
  readonly configInitialized: RefObject<boolean>;
  readonly initializeDefaultConfigValues: () => void;
  readonly toolInputVariables: Record<string, unknown>;
  readonly onChangeInputVariables: (value: Record<string, unknown>) => void;
  readonly isValidForm?: boolean | undefined;
  readonly changesDisabled?: boolean | undefined;
  readonly index: IndexRow | null | undefined;
  readonly isRunningTool?: boolean | undefined;
  readonly ToolFormField: ComponentType<ToolFormFieldProps>;
}

export function IndexViews(props: IndexViewsProps): ReactNode {
  const {
    activeView,
    schema,
    toolsConfig,
    configInitialized,
    initializeDefaultConfigValues,
    toolInputVariables,
    onChangeInputVariables,
    isValidForm,
    changesDisabled,
    index,
    isRunningTool,
    ToolFormField,
  } = props;

  const commonConfigProps = useMemo(
    () => ({
      schema,
      configInitialized,
      initializeDefaultConfigValues,
      toolInputVariables,
      onChangeInputVariables,
      changesDisabled,
      withNavigation: true,
      ToolFormField,
    }),
    [configInitialized, initializeDefaultConfigValues, onChangeInputVariables, schema, toolInputVariables, changesDisabled, ToolFormField],
  );

  if (activeView === EditViewTabsEnum.configuration) {
    return (
      <IndexConfig
        sx={{ '.index-config-field:first-of-type': { marginTop: 0 } }}
        {...commonConfigProps}
      />
    );
  }

  if (activeView === EditViewTabsEnum.history) {
    return <IndexHistory history={(index?.metadata['history'] as readonly IndexHistoryItem[] | undefined) ?? []} />;
  }

  return (
    <IndexConfig
      toolsConfig={toolsConfig}
      isValidForm={isValidForm}
      isRunningTool={isRunningTool}
      {...commonConfigProps}
    />
  );
}
