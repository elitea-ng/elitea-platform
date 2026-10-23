//go:build handoff_live

// Command handoff-live-cloudturn plays the cloud executor for the desktop's
// live handoff tests (apps/elitea-desktop/src-tauri/src/handoff_live_tests.rs,
// IPC.md "Cloud handoff: live run") until the worker serves workspace turns.
//
// It is NOT part of the product and is built only with the handoff_live tag:
//
//	go build -tags handoff_live -o cloudturn-sim ./cmd/handoff-live-cloudturn
//
// It lives under services/elitea-main because it drives Main's own
// application services (workspacehandoff, workspacestore), which are
// internal packages and cannot be imported from another module.
//
// One cloud turn:
//
//	cloudturn-sim -project P -conversation UUID -bundle FILE -commit SHA [-hold FILE]
//
// writes a question and an answer message, starts the cloud turn on the
// answer (CLOUD_IDLE -> CLOUD_RUNNING), uploads FILE and publishes
// refs/elitea/cloud = SHA as the cloud_executor, waits for the file named by
// -hold to exist (when given), and ends the turn (FinishCloudTurn: CLOUD_IDLE,
// or RECLAIMING when a take back is pending). It prints one JSON object:
// {"state","epoch","cloud_sha","seq","execution_id","answer_uuid","answer_meta"}.
//
//	cloudturn-sim -sql QUERY
//
// prints the rows of QUERY as a JSON array of arrays of strings (NULL is "").
//
// Environment, as Main's own: DATABASE_URL and the STORAGE_* / S3_* variables
// of an S3 backend (storage.ConfigFromEnv).
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage/s3"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

const cloudRef = "refs/elitea/cloud"

func main() {
	project := flag.Int64("project", 0, "project id")
	conversation := flag.String("conversation", "", "conversation uuid")
	bundle := flag.String("bundle", "", "the git bundle of the cloud turn (every ref plus refs/elitea/cloud)")
	commit := flag.String("commit", "", "the commit refs/elitea/cloud moves to")
	hold := flag.String("hold", "", "end the turn only after this file exists")
	query := flag.String("sql", "", "print the rows of this query as JSON and exit")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := run(ctx, *project, *conversation, *bundle, *commit, *hold, *query); err != nil {
		fmt.Fprintln(os.Stderr, "cloudturn-sim:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, project int64, conversation, bundle, commit, hold, query string) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	if query != "" {
		return printRows(ctx, dsn, query)
	}
	if project <= 0 || !workspacestore.ValidUUID(conversation) || bundle == "" || commit == "" {
		return errors.New("-project, -conversation, -bundle and -commit are required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	handoff, store, err := compose(ctx, pool)
	if err != nil {
		return err
	}
	thread := workspacestore.Thread{ProjectID: project, ConversationUUID: conversation}

	var epoch int64
	if err := pool.QueryRow(ctx, `SELECT epoch FROM elitea_runtime.workspace_handoffs
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND deleted_at IS NULL`, project, conversation).Scan(&epoch); err != nil {
		return fmt.Errorf("read the handoff epoch: %w", err)
	}
	answer, err := writeQuestionAndAnswer(ctx, pool, project, conversation)
	if err != nil {
		return err
	}
	executionID, err := newExecutionID()
	if err != nil {
		return err
	}
	if _, _, err := handoff.StartCloudTurn(ctx, thread, epoch, executionID, answer); err != nil {
		return fmt.Errorf("start the cloud turn: %w", err)
	}
	caller := workspacestore.Caller{Writer: workspacestore.WriterCloudExecutor, ExecutionID: executionID}
	seq, err := publishCloudRef(ctx, store, thread, caller, epoch, bundle, commit)
	if err != nil {
		return err
	}
	if hold != "" {
		if err := waitFor(ctx, hold); err != nil {
			return err
		}
	}
	finished, err := handoff.FinishCloudTurn(ctx, thread, epoch, executionID)
	if err != nil {
		return fmt.Errorf("finish the cloud turn: %w", err)
	}
	var meta map[string]any
	var raw []byte
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT meta FROM p_%d.chat_message_group WHERE uuid = $1::uuid`, project),
		answer).Scan(&raw); err != nil {
		return fmt.Errorf("read the answer's meta: %w", err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return fmt.Errorf("decode the answer's meta: %w", err)
	}
	cloudSHA := ""
	if finished.CloudSHA != nil {
		cloudSHA = *finished.CloudSHA
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"state": string(finished.State), "epoch": finished.Epoch, "cloud_sha": cloudSHA, "seq": seq,
		"execution_id": executionID, "answer_uuid": answer, "answer_meta": meta,
	})
}

// compose builds the two services over the pool as Main's composition does
// (cmd/elitea-main/workspace_handoff.go), with a stub device policy that
// allows the handoff and no audit trail or turn canceller.
func compose(ctx context.Context, pool *pgxpool.Pool) (*workspacehandoff.Service, *workspacestore.Service, error) {
	cfg, err := storage.ConfigFromEnv(os.LookupEnv)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Backend != "s3" {
		return nil, nil, fmt.Errorf("only the s3 backend is supported, not %q", cfg.Backend)
	}
	backend, err := s3.New(ctx, s3.Config{
		Endpoint: cfg.S3.Endpoint, AccessKey: cfg.S3.AccessKey, SecretKey: cfg.S3.SecretKey,
		Region: cfg.S3.Region, ForcePathStyle: cfg.S3.ForcePathStyle, Bucket: cfg.S3.Bucket, KeyPrefix: cfg.S3.KeyPrefix,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("object store: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	settings, err := dbrepos.NewCloudExecutorSettingsRepository(pool)
	if err != nil {
		return nil, nil, err
	}
	policy, err := workspacehandoff.NewPolicyService(settings, settings, allowHandoff{}, true,
		platformconfig.CloudExecutorResources{TurnWallSeconds: 3600})
	if err != nil {
		return nil, nil, err
	}
	storeRepo, err := dbrepos.NewWorkspaceStoreRepository(pool)
	if err != nil {
		return nil, nil, err
	}
	buckets := workspacestore.Buckets{Default: workspacestore.PhysicalStore{Name: cfg.Container, Store: backend}}
	store, err := workspacestore.NewService(storeRepo, buckets, policy, discardAudit{}, logger)
	if err != nil {
		return nil, nil, err
	}
	handoffRepo, err := dbrepos.NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		return nil, nil, err
	}
	handoff, err := workspacehandoff.NewService(handoffRepo, policy, store, noCanceller{}, discardRecorder{}, logger)
	if err != nil {
		return nil, nil, err
	}
	return handoff, store, nil
}

type allowHandoff struct{}

func (allowHandoff) Policy(context.Context) (platformconfig.NativeClientPolicy, error) {
	var policy platformconfig.NativeClientPolicy
	policy.LocalWork.Allowed = true
	policy.LocalWork.CloudHandoff = true
	return policy, nil
}

type discardAudit struct{}

func (discardAudit) StoreEvent(context.Context, workspacestore.AuditEvent) {}

type discardRecorder struct{}

func (discardRecorder) Record(context.Context, audit.Event) {}

type noCanceller struct{}

func (noCanceller) CancelCloudTurn(context.Context, int64, int64, string) error {
	return errors.New("the cloud turn simulator cannot cancel a turn")
}

// publishCloudRef uploads the bundle and moves refs/elitea/cloud to commit as
// the executor, from the value the plan shows (nil before the first turn).
func publishCloudRef(
	ctx context.Context, store *workspacestore.Service, thread workspacestore.Thread, caller workspacestore.Caller,
	epoch int64, bundle, commit string,
) (int64, error) {
	bytes, err := os.ReadFile(bundle)
	if err != nil {
		return 0, fmt.Errorf("read the bundle: %w", err)
	}
	sum := sha256.Sum256(bytes)
	sha := hex.EncodeToString(sum[:])
	plan, err := store.Plan(ctx, thread, caller, 0)
	if err != nil {
		return 0, fmt.Errorf("read the plan: %w", err)
	}
	var old *string
	if current, ok := plan.Refs[cloudRef]; ok {
		old = &current
	}
	if _, err := store.UploadSegment(ctx, thread, caller, epoch, sha, int64(len(bytes)), readerOf(bytes)); err != nil {
		return 0, fmt.Errorf("upload the segment: %w", err)
	}
	published, err := store.Publish(ctx, workspacestore.PublishRequest{
		Thread: thread, Caller: caller, Epoch: epoch, Kind: "COMPACT", SHA256: sha, Bytes: int64(len(bytes)),
		Refs:         []workspacestore.RefUpdate{{Name: cloudRef, Old: old, New: commit}},
		ObjectFormat: workspacestore.ObjectFormatSHA1,
	})
	if err != nil {
		return 0, fmt.Errorf("publish %s: %w", cloudRef, err)
	}
	return published.Seq, nil
}

type byteReader struct {
	data []byte
	off  int
}

func readerOf(data []byte) io.Reader { return &byteReader{data: data} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// writeQuestionAndAnswer adds the turn's two message groups to the thread as a
// local turn's commit does (repos/local_turns.go): the question by the
// conversation's user participant, the answer by its application (or dummy)
// participant, paired by reply_to_id, the answer's task_id empty. It returns
// the answer's uuid.
func writeQuestionAndAnswer(ctx context.Context, pool *pgxpool.Pool, project int64, conversation string) (string, error) {
	schema := fmt.Sprintf("p_%d", project)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var conversationID, userID, answeringID int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT conversation.id, author.id, answering.id
FROM %[1]s.chat_conversations AS conversation
JOIN %[1]s.chat_participant_mapping AS author_mapping ON author_mapping.conversation_id = conversation.id
JOIN %[1]s.chat_participants AS author ON author.id = author_mapping.participant_id AND author.entity_name = 'user'
JOIN LATERAL (
    SELECT participant.id
    FROM %[1]s.chat_participant_mapping AS mapping
    JOIN %[1]s.chat_participants AS participant ON participant.id = mapping.participant_id
    WHERE mapping.conversation_id = conversation.id AND participant.entity_name IN ('application', 'dummy')
    ORDER BY (participant.entity_name = 'application') DESC, participant.id
    LIMIT 1
) AS answering ON TRUE
WHERE conversation.uuid = $1::uuid
ORDER BY author.id
LIMIT 1`, schema), conversation).Scan(&conversationID, &userID, &answeringID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("the conversation has no user participant and answering participant")
	}
	if err != nil {
		return "", fmt.Errorf("resolve the participants: %w", err)
	}
	var questionID int64
	var questionAt time.Time
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %[1]s.chat_message_group
    (uuid, author_participant_id, conversation_id, sent_to_id, meta, is_streaming, created_at)
VALUES ($1::uuid, $2, $3, $4, '{}'::jsonb, FALSE, GREATEST(
    clock_timestamp() AT TIME ZONE 'UTC',
    (SELECT max(created_at) + interval '1 millisecond' FROM %[1]s.chat_message_group WHERE conversation_id = $3)))
RETURNING id, created_at`, schema), uuid.NewString(), userID, conversationID, answeringID).Scan(&questionID, &questionAt); err != nil {
		return "", fmt.Errorf("insert the question: %w", err)
	}
	if err := insertText(ctx, tx, schema, questionID, "change the README"); err != nil {
		return "", err
	}
	answer := uuid.NewString()
	var answerID int64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %[1]s.chat_message_group
    (uuid, author_participant_id, conversation_id, reply_to_id, meta, is_streaming, created_at)
VALUES ($1::uuid, $2, $3, $4, '{}'::jsonb, FALSE, $5::timestamp + interval '1 millisecond')
RETURNING id`, schema), answer, answeringID, conversationID, questionID, questionAt).Scan(&answerID); err != nil {
		return "", fmt.Errorf("insert the answer: %w", err)
	}
	if err := insertText(ctx, tx, schema, answerID, "Done: README.md and src/cloud.rs changed."); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return answer, nil
}

func insertText(ctx context.Context, tx pgx.Tx, schema string, groupID int64, content string) error {
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
WITH item AS (
    INSERT INTO %[1]s.chat_message_items (uuid, item_type, order_index, meta, message_group_id, created_at)
    VALUES (gen_random_uuid(), 'text_message', 0, '{}'::jsonb, $1, now() AT TIME ZONE 'UTC')
    RETURNING id
)
INSERT INTO %[1]s.chat_messages_text (id, content) SELECT item.id, $2 FROM item`, schema), groupID, content); err != nil {
		return fmt.Errorf("insert the message text: %w", err)
	}
	return nil
}

func newExecutionID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// waitFor blocks until the file exists.
func waitFor(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", path, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// printRows prints the rows of query as JSON strings (NULL is "").
func printRows(ctx context.Context, dsn, query string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := [][]string{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return err
		}
		row := make([]string, len(values))
		for i, value := range values {
			row[i] = text(value)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

func text(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case bool, int16, int32, int64, float32, float64:
		return fmt.Sprint(v)
	default:
		if encoded, err := json.Marshal(v); err == nil {
			return string(encoded)
		}
		return fmt.Sprint(v)
	}
}
