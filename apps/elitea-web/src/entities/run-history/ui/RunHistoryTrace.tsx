/** Existing trace reads optionally narrow to one original admitted Test receipt. */
import { useState, type ReactNode } from "react";

import Box from "@mui/material/Box";
import CircularProgress from "@mui/material/CircularProgress";

import {
  useGetMessageTrace,
  useListMessageTraces,
} from "@/shared/api/generated/chat/chat";
import type {
  EditorTestRun,
  ListMessageTracesParams,
  MessageTraceStep,
  MessageTraceStepDetail,
} from "@/shared/api/generated/model";
import { t } from "@/shared/i18n";
import { NoResultsMessage } from "@/shared/ui/NoResultsMessage";

import { editorTestRunKey } from "./RunHistoryList";
import { RunHistoryTraceSteps } from "./RunHistoryTraceSteps";

export interface RunHistoryTraceProps {
  readonly projectId: string;
  readonly conversationId: string;
  readonly editorTestRun?: EditorTestRun;
}

function identityParams(run: EditorTestRun | undefined): {
  readonly execution_id?: string;
  readonly execution_generation?: string;
  readonly response_message_id?: string;
} {
  if (run === undefined) return {};
  return {
    execution_id: run.execution_id,
    execution_generation: run.execution_generation,
    response_message_id: run.response_message_id,
  };
}

function matchingSteps(
  rows: readonly MessageTraceStep[] | undefined,
  run: EditorTestRun | undefined,
): readonly MessageTraceStep[] {
  return (rows ?? []).filter(
    (step) =>
      run === undefined ||
      step.message_group_id === run.response_message_group_id,
  );
}

function matchingDetail(
  candidate: MessageTraceStepDetail | undefined,
  selected: MessageTraceStep | undefined,
): MessageTraceStepDetail | undefined {
  return candidate?.id === selected?.id &&
    candidate?.message_group_id === selected?.message_group_id
    ? candidate
    : undefined;
}

function listParams(run: EditorTestRun | undefined): ListMessageTracesParams {
  return {
    include_total: true,
    ...identityParams(run),
    ...(run === undefined
      ? {}
      : { message_group_id: run.response_message_group_id }),
  };
}

function RunHistoryTraceBody({
  projectId,
  conversationId,
  editorTestRun,
}: RunHistoryTraceProps): ReactNode {
  const [selectedStep, setSelectedStep] = useState<MessageTraceStep>();
  const tracesQuery = useListMessageTraces(
    projectId,
    Number(conversationId),
    listParams(editorTestRun),
    { query: { enabled: projectId !== "" && conversationId !== "" } },
  );
  const listing = tracesQuery.data?.data as
    { readonly rows: readonly MessageTraceStep[] } | undefined;
  const steps = matchingSteps(listing?.rows, editorTestRun);
  const detailQuery = useGetMessageTrace(
    projectId,
    selectedStep?.id ?? 0,
    {
      message_group_id: selectedStep?.message_group_id ?? 0,
      ...identityParams(editorTestRun),
    },
    { query: { enabled: selectedStep !== undefined } },
  );
  const detail = matchingDetail(
    detailQuery.data?.data as MessageTraceStepDetail | undefined,
    selectedStep,
  );
  if (tracesQuery.isPending) {
    return (
      <Box
        sx={{ display: "flex", justifyContent: "center", padding: "1rem" }}
        data-testid="run-history-trace-loading"
      >
        <CircularProgress size={24} />
      </Box>
    );
  }
  if (steps.length === 0) {
    return (
      <NoResultsMessage
        title={t("entities.runHistory.trace.emptyTitle", "No trace steps")}
        description={t(
          "entities.runHistory.trace.empty",
          "This conversation has no recorded tool calls or thinking steps.",
        )}
      />
    );
  }
  return (
    <RunHistoryTraceSteps
      projectId={projectId}
      scopeKey={conversationId}
      steps={steps}
      selectedStep={selectedStep}
      detail={detail}
      onSelect={setSelectedStep}
    />
  );
}

/** Scope changes discard details; older receipts never fall back to conversation traces. */
export function RunHistoryTrace(props: RunHistoryTraceProps): ReactNode {
  const run = props.editorTestRun;
  if (
    run !== undefined &&
    (run.trace_available !== true ||
      run.response_message_group_id === undefined)
  ) {
    return (
      <NoResultsMessage
        title={t(
          "entities.runHistory.trace.runUnavailable",
          "No recorded trace for this run",
        )}
        description={t(
          "entities.runHistory.trace.runUnavailableDetail",
          "Only trace steps recorded for this original run are shown.",
        )}
      />
    );
  }
  const key = `${props.projectId}:${props.conversationId}:${run === undefined ? "conversation" : `${editorTestRunKey(run)}:${String(run.response_message_group_id)}`}`;
  return <RunHistoryTraceBody key={key} {...props} />;
}
