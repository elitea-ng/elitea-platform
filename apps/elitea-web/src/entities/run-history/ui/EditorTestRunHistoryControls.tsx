import type { ReactNode } from "react";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import CircularProgress from "@mui/material/CircularProgress";
import { t } from "@/shared/i18n";

const PAGE_SIZE = 50;
const MAX_RUN_OFFSET = 10000;

export function HistoryLoading(): ReactNode {
  return (
    <Box data-testid="run-history-loading" sx={{ padding: "2rem" }}>
      <CircularProgress size={28} />
    </Box>
  );
}

export function PageControls({
  offset,
  hasMore,
  onPage,
  bounded = false,
}: {
  readonly offset: number;
  readonly hasMore: boolean;
  readonly onPage: (offset: number) => void;
  readonly bounded?: boolean;
}): ReactNode {
  return (
    <Box
      sx={{ display: "flex", gap: "0.75rem" }}
      data-testid={
        bounded ? "editor-test-run-pages" : "editor-test-context-pages"
      }
    >
      <Button
        disabled={offset === 0}
        onClick={() => onPage(offset - PAGE_SIZE)}
      >
        {t("entities.runHistory.previous", "Previous")}
      </Button>
      <Button
        disabled={!hasMore || (bounded && offset + PAGE_SIZE > MAX_RUN_OFFSET)}
        onClick={() => onPage(offset + PAGE_SIZE)}
      >
        {t("entities.runHistory.next", "Next")}
      </Button>
    </Box>
  );
}
