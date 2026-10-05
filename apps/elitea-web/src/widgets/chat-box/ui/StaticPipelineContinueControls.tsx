import { t } from "@/shared/i18n";
import {
  useStaticPipelineContinuation,
  type StaticContinuationParams,
} from "./hooks/useStaticPipelineContinuation";
import type { ChatBoxProps } from "./ChatBox.types";
/** Authored pauses have Continue controls separate from dynamic approval cards. */
import { useState } from "react";
import {
  Alert,
  Box,
  Button,
  Checkbox,
  FormControlLabel,
  Stack,
  TextField,
  Typography,
} from "@mui/material";
import type { StaticPauseBinding } from "@/features/chat-messages";
interface Props {
  readonly pause: StaticPauseBinding;
  readonly busy: boolean;
  readonly error?: string | undefined;
  readonly onContinue: (
    text: string,
    selected: readonly string[],
  ) => Promise<void>;
}
export function StaticPipelineContinueControls({
  pause,
  busy,
  error,
  onContinue,
}: Props) {
  const [text, setText] = useState(
    t("widgets.chatBox.staticPause.defaultMessage", "Continue"),
  );
  const [selected, setSelected] = useState<readonly string[]>([]);
  return (
    <Box
      component="section"
      aria-label={t(
        "widgets.chatBox.staticPause.region",
        "Static pipeline continuation",
      )}
      sx={{ p: 2 }}
    >
      <Stack spacing={1}>
        <Typography>
          {pause.kind === "root"
            ? t(
                "widgets.chatBox.staticPause.rootTitle",
                "Paused {{kind}} {{node}}",
                { kind: pause.proof.kind, node: pause.proof.node_name },
              )
            : t(
                "widgets.chatBox.staticPause.toolsTitle",
                "Paused saved pipelines",
              )}
        </Typography>
        {pause.kind === "tools" &&
          pause.inventory.pauses.map((leaf) => (
            <FormControlLabel
              key={leaf.proof.pause_id}
              control={
                <Checkbox
                  disabled={busy}
                  checked={selected.includes(leaf.proof.pause_id)}
                  onChange={(_, checked) =>
                    setSelected((current) =>
                      checked
                        ? [...current, leaf.proof.pause_id]
                        : current.filter((id) => id !== leaf.proof.pause_id),
                    )
                  }
                />
              }
              label={`${leaf.proof.node_name} (${leaf.proof.kind}) · ${leaf.child_thread_id}`}
            />
          ))}
        <TextField
          label={t(
            "widgets.chatBox.staticPause.message",
            "Continuation message",
          )}
          value={text}
          disabled={busy}
          onChange={(event) => setText(event.target.value)}
          multiline
          maxRows={4}
        />
        {error && <Alert severity="error">{error}</Alert>}
        <Button
          disabled={
            busy || !text.trim() || (pause.kind === "tools" && !selected.length)
          }
          onClick={() => {
            void onContinue(text, selected);
          }}
        >
          {pause.kind === "root"
            ? t("widgets.chatBox.staticPause.rootContinue", "Continue pipeline")
            : t(
                "widgets.chatBox.staticPause.toolsContinue",
                "Continue selected pipelines",
              )}
        </Button>
      </Stack>
    </Box>
  );
}

type HostProps = Omit<StaticContinuationParams, "restoredRun"> & {
  readonly editorTest: NonNullable<ChatBoxProps["extensions"]>["editorTest"];
};
/** Shared composition for main chat and Pipeline Test; mounting submits no work. */
export function StaticPipelineContinuation({
  editorTest,
  ...params
}: HostProps) {
  const controls = useStaticPipelineContinuation({
    ...params,
    restoredRun: editorTest?.restoredRun,
  });
  return controls.pause ? (
    <StaticPipelineContinueControls
      key={controls.key}
      pause={controls.pause}
      busy={controls.busy}
      error={controls.error}
      onContinue={controls.submit}
    />
  ) : null;
}
