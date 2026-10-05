/** Durable editor Test receipts reuse conversation admission and run projection. */
import { useState, type ReactNode } from "react";

import CloseIcon from "@mui/icons-material/Close";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import IconButton from "@mui/material/IconButton";
import List from "@mui/material/List";
import ListItemButton from "@mui/material/ListItemButton";
import ListItemText from "@mui/material/ListItemText";
import Typography from "@mui/material/Typography";

import {
  useGetConversation,
  useListConversations,
} from "@/shared/api/generated/chat/chat";
import {
  EditorTestContext,
  EditorTestRunsPage,
  type ConversationDetail,
  type ConversationListing,
  type ConversationSummary,
  type EditorTestRun,
} from "@/shared/api/generated/model";
import { unwrapBody } from "@/shared/api/unwrap";
import { t } from "@/shared/i18n";
import { NoResultsMessage } from "@/shared/ui/NoResultsMessage";

import { EditorTestRunHistoryList, editorTestRunKey } from "./RunHistoryList";
import type { RunHistoryPanelProps } from "./RunHistoryPanel";
import { RunHistoryTrace } from "./RunHistoryTrace";
import { HistoryLoading, PageControls } from "./EditorTestRunHistoryControls";

const PAGE_SIZE = 50;
// Receipts can settle while History is closed. Read each mounted page again.
const HISTORY_QUERY_OPTIONS = {
  query: { staleTime: 0, refetchOnMount: "always" },
} as const;

function testContext(
  meta: unknown,
  projectId: string,
  applicationId: string,
): EditorTestContext | undefined {
  if (typeof meta !== "object" || meta === null) return undefined;
  const record = meta as Record<string, unknown>;
  const parsed = EditorTestContext.safeParse(record["editor_test"]);
  if (!parsed.success || record["is_hidden"] !== true) return undefined;
  const identity = parsed.data;
  return identity.project_id === projectId &&
    identity.application_id === applicationId
    ? identity
    : undefined;
}

function runPage(
  detail: ConversationDetail | undefined,
  projectId: string,
  applicationId: string,
  row: ConversationSummary,
): EditorTestRunsPage | undefined {
  const scope: { readonly source?: unknown; readonly is_private?: unknown } | undefined = detail;
  if (detail === undefined || scope?.source !== "editor_test" || scope.is_private !== true)
    return undefined;
  const actual = testContext(detail.meta, projectId, applicationId);
  const expected = testContext(row.meta, projectId, applicationId);
  if (
    String(detail.id) !== String(row.id) ||
    actual === undefined ||
    expected === undefined
  )
    return undefined;
  if (
    actual.actor_id !== expected.actor_id ||
    actual.application_version_id !== expected.application_version_id
  )
    return undefined;
  const parsed = EditorTestRunsPage.safeParse(detail.editor_test_runs);
  return parsed.success ? parsed.data : undefined;
}

function RunSelection({
  run,
  projectId,
  conversationId,
}: {
  readonly run: EditorTestRun | undefined;
  readonly projectId: string;
  readonly conversationId: string;
}): ReactNode {
  if (run === undefined) {
    return (
      <Typography data-testid="run-history-no-selection" color="text.secondary">
        {t(
          "entities.runHistory.selectPrompt",
          "Select a run to see its trace.",
        )}
      </Typography>
    );
  }
  return (
    <RunHistoryTrace
      projectId={projectId}
      conversationId={conversationId}
      editorTestRun={run}
    />
  );
}

function ContextRuns({
  row,
  projectId,
  applicationId,
}: {
  readonly row: ConversationSummary;
  readonly projectId: string;
  readonly applicationId: string;
}): ReactNode {
  const [offset, setOffset] = useState(0);
  const [selection, setSelection] = useState<string>();
  const detailQuery = useGetConversation(
    projectId,
    String(row.id),
    {
      editor_test_runs: true,
      runs_limit: PAGE_SIZE,
      runs_offset: offset,
      messages_limit: 0,
    },
    HISTORY_QUERY_OPTIONS,
  );
  const page = runPage(
    unwrapBody(detailQuery.data) as ConversationDetail | undefined,
    projectId,
    applicationId,
    row,
  );
  const selected = page?.rows.find(
    (run) => editorTestRunKey(run) === selection,
  );
  if (detailQuery.isPending) return <HistoryLoading />;
  if (detailQuery.isError || page === undefined) {
    return (
      <NoResultsMessage
        title={t("entities.runHistory.unavailable", "Run history unavailable")}
        description={t(
          "entities.runHistory.unavailableDetail",
          "This Test history could not be loaded.",
        )}
      />
    );
  }
  const onPage = (next: number): void => {
    setSelection(undefined);
    setOffset(next);
  };
  return (
    <Box
      sx={{
        display: "flex",
        flexDirection: "column",
        gap: "0.75rem",
        minHeight: 0,
      }}
    >
      <Box sx={{ display: "flex", gap: "1.5rem", minHeight: 0 }}>
        <Box sx={{ flex: 1, minWidth: 0 }}>
          <EditorTestRunHistoryList
            rows={page.rows}
            selectedKey={selection}
            onSelect={(run) => setSelection(editorTestRunKey(run))}
          />
          {page.rows.length === 0 && (
            <Typography>
              {t(
                "entities.runHistory.noTestRuns",
                "No admitted Test runs yet.",
              )}
            </Typography>
          )}
        </Box>
        <Box sx={{ flex: 1, minWidth: 0 }}>
          <RunSelection
            run={selected}
            projectId={projectId}
            conversationId={String(row.id)}
          />
        </Box>
      </Box>
      <PageControls
        offset={offset}
        hasMore={page.has_more}
        onPage={onPage}
        bounded
      />
    </Box>
  );
}

function ContextList({
  rows,
  selected,
  onSelect,
  versions,
}: {
  readonly rows: readonly ConversationSummary[];
  readonly selected: ConversationSummary | undefined;
  readonly onSelect: (id: number) => void;
  readonly versions: RunHistoryPanelProps["versions"];
}): ReactNode {
  return (
    <List dense data-testid="editor-test-contexts">
      {rows.map((row) => {
        const identity = EditorTestContext.safeParse(row.meta?.["editor_test"]);
        const versionId = identity.success
          ? identity.data.application_version_id
          : "";
        const name =
          versions?.find((version) => String(version.id) === versionId)?.name ??
          versionId;
        return (
          <ListItemButton
            key={row.id}
            selected={row.id === selected?.id}
            onClick={() => onSelect(row.id)}
            data-testid="editor-test-context"
          >
            <ListItemText
              primary={row.name}
              secondary={t(
                "entities.runHistory.savedVersion",
                "Saved version {{version}}",
                { version: name },
              )}
            />
          </ListItemButton>
        );
      })}
    </List>
  );
}

function contextRows(
  listing: ConversationListing | undefined,
  projectId: string,
  applicationId: string,
): readonly ConversationSummary[] {
  return (listing?.rows ?? []).filter(
    (row) => testContext(row.meta, projectId, applicationId) !== undefined,
  );
}

function selectedContext(
  rows: readonly ConversationSummary[],
  selectedId: number | undefined,
): ConversationSummary | undefined {
  return rows.find((row) => row.id === selectedId) ?? rows[0];
}

function TestHistoryBody(
  props: RunHistoryPanelProps & {
    readonly projectId: string;
    readonly applicationId: string;
  },
): ReactNode {
  const [offset, setOffset] = useState(0);
  const [selectedId, setSelectedId] = useState<number>();
  const query = useListConversations(
    props.projectId,
    {
      entity_name: "application",
      entity_meta_id: props.applicationId,
      source: "editor_test",
      hidden: "only",
      mine: true,
      limit: PAGE_SIZE,
      offset,
    },
    HISTORY_QUERY_OPTIONS,
  );
  const listing = query.data?.data as ConversationListing | undefined;
  const rows = contextRows(listing, props.projectId, props.applicationId);
  const selected = selectedContext(rows, selectedId);
  if (query.isPending) return <HistoryLoading />;
  if (query.isError)
    return (
      <NoResultsMessage
        title={t("entities.runHistory.unavailable", "Run history unavailable")}
        description={t(
          "entities.runHistory.unavailableDetail",
          "This Test history could not be loaded.",
        )}
      />
    );
  if (rows.length === 0 && offset === 0)
    return (
      <NoResultsMessage
        title={t("entities.runHistory.emptyTitle", "No runs yet")}
        description={t(
          "entities.runHistory.noTestRuns",
          "No admitted Test runs yet.",
        )}
      />
    );
  const onPage = (next: number): void => {
    setSelectedId(undefined);
    setOffset(next);
  };
  return (
    <>
      <ContextList
        rows={rows}
        selected={selected}
        onSelect={setSelectedId}
        versions={props.versions}
      />
      <PageControls
        offset={offset}
        hasMore={(listing?.total ?? 0) > offset + PAGE_SIZE}
        onPage={onPage}
      />
      {selected !== undefined && props.onRestoreConversation !== undefined && (
        <Button
          data-testid="editor-test-restore-context"
          onClick={() => props.onRestoreConversation?.(String(selected.id))}
        >
          {t("entities.runHistory.restoreTest", "Restore Test")}
        </Button>
      )}
      {selected !== undefined && (
        <ContextRuns
          key={selected.id}
          row={selected}
          projectId={props.projectId}
          applicationId={props.applicationId}
        />
      )}
    </>
  );
}

export function EditorTestRunHistoryPanel(
  props: RunHistoryPanelProps,
): ReactNode {
  return (
    <Box
      data-testid="run-history-panel"
      sx={{
        display: "flex",
        flexDirection: "column",
        gap: "0.75rem",
        height: "100%",
        overflowY: "auto",
      }}
    >
      <Box
        sx={{
          display: "flex",
          alignItems: "center",
          justifyContent: "space-between",
        }}
      >
        <Typography variant="headingSmall">
          {t("entities.runHistory.title", "Run history")}
        </Typography>
        <IconButton
          data-testid="run-history-close"
          aria-label={t("entities.runHistory.close", "Close run history")}
          onClick={props.onClose}
        >
          <CloseIcon fontSize="small" />
        </IconButton>
      </Box>
      {props.projectId === undefined || props.entityId === undefined ? (
        <HistoryLoading />
      ) : (
        <TestHistoryBody
          key={`${props.projectId}:${String(props.entityId)}`}
          {...props}
          projectId={props.projectId}
          applicationId={String(props.entityId)}
        />
      )}
    </Box>
  );
}
