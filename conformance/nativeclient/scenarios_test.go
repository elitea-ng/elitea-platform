//go:build conformance

package nativeclient_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/conformance/nativeclient/client"
)

// ── 3. Refresh rotation, re-delivery and reuse ───────────────────────────────

func (s *suite) refreshRotation(t *testing.T) {
	ctx := testContext(t, 3*time.Minute)
	s.requireSession(t)
	device := s.cfg.signIn(t, s.discovery, s.cfg.User, "oidc", "conformance refresh")
	original := device.tokens

	first := device.refresh(ctx, t)
	if first.RefreshToken == original.RefreshToken || first.AccessToken == original.AccessToken {
		t.Fatal("a refresh did not rotate both tokens")
	}
	if first.DeviceID != original.DeviceID {
		t.Errorf("a refresh moved the device: %q → %q", original.DeviceID, first.DeviceID)
	}
	if response := need(t)(device.api.Get(ctx, client.DevicesPath)); response.Status != http.StatusOK {
		t.Fatalf("the rotated access token: %s", response)
	}

	// Re-delivery (coordinator decision 7): the immediately previous refresh
	// token, inside the window, answers the SAME successor pair, so a
	// response lost on a mobile network cannot wipe the device.
	redelivered, err := device.api.Refresh(ctx, s.discovery.NativeAuth.TokenEndpoint, s.cfg.ClientID, original.RefreshToken)
	if err != nil {
		t.Fatalf("re-delivery of the previous refresh token: %v", err)
	}
	if redelivered.RefreshToken != first.RefreshToken || redelivered.AccessToken != first.AccessToken {
		t.Fatal("re-delivery minted a new pair instead of answering the same successor")
	}

	second := device.refresh(ctx, t)
	// Reuse: the token two generations back is no longer "the immediately
	// previous one". Presenting it is theft evidence: the whole family goes.
	_, err = device.api.Refresh(ctx, s.discovery.NativeAuth.TokenEndpoint, s.cfg.ClientID, original.RefreshToken)
	var refused *client.TokenError
	if !errors.As(err, &refused) || !client.IsDeviceRevoked(refused.Response) {
		t.Fatalf("reuse of a superseded refresh token: %v, want 401 device_revoked", err)
	}
	if response := need(t)(device.api.Get(ctx, client.DevicesPath)); !client.IsDeviceRevoked(response) {
		t.Errorf("the family's newest access token after reuse: %s, want 401 device_revoked", response)
	}
	if _, err := device.api.Refresh(ctx, s.discovery.NativeAuth.TokenEndpoint, s.cfg.ClientID, second.RefreshToken); !errors.As(err, &refused) ||
		!client.IsDeviceRevoked(refused.Response) {
		t.Errorf("the family's newest refresh token after reuse: %v, want 401 device_revoked", err)
	}
	// The user sees the device as revoked, with a reason.
	listing := mustJSON(t, need(t)(s.main.api.Get(ctx, client.DevicesPath+"?include_revoked=true")), http.StatusOK)
	if row := deviceRow(listing, original.DeviceID); row == nil || row["revoked_at"] == nil || row["revoke_reason"] == nil {
		t.Errorf("the reused device is not listed as revoked with a reason: %v", row)
	}
	s.done(t)
}

func deviceRow(listing map[string]any, deviceID string) map[string]any {
	rows, _ := listing["devices"].([]any)
	for _, raw := range rows {
		if row, _ := raw.(map[string]any); row["id"] == deviceID {
			return row
		}
	}
	return nil
}

// ── 4. Chat send is idempotent on question_id ────────────────────────────────

func newUUID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// personalProject finds the persona's personal project in the project list,
// the one the /llm hop resolves the model credential from.
func (s *suite) personalProject(ctx context.Context, t *testing.T) int64 {
	t.Helper()
	if raw := env("ELITEA_CONFORMANCE_PROJECT_ID", ""); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatalf("ELITEA_CONFORMANCE_PROJECT_ID=%q", raw)
		}
		return id
	}
	response := need(t)(s.main.api.Get(ctx, client.ProjectsPath))
	if response.Status != http.StatusOK {
		t.Fatalf("project list: %s", response)
	}
	var projects []map[string]any
	if err := response.JSON(&projects); err != nil {
		t.Fatal(err)
	}
	for _, project := range projects {
		name, _ := project["name"].(string)
		if strings.HasPrefix(name, "project_user_") {
			if id, ok := asInt64(project["id"]); ok {
				return id
			}
		}
	}
	t.Fatalf("the project list has no personal project (project_user_*): %s", response)
	return 0
}

func (s *suite) createConversation(ctx context.Context, t *testing.T, name string) (id, uuid string) {
	t.Helper()
	created := mustJSON(t, need(t)(s.main.api.Do(ctx, http.MethodPost, client.ConversationPath(s.projectID),
		map[string]any{"name": name}, nil)), http.StatusCreated)
	id, _ = created["id"].(string)
	uuid, _ = created["uuid"].(string)
	if id == "" || uuid == "" {
		t.Fatalf("created conversation has no id/uuid: %v", created)
	}
	return id, uuid
}

func (s *suite) chatBody(prompt, questionID string) map[string]any {
	return map[string]any{
		"project_id":        s.projectID,
		"conversation_uuid": s.conversationUUID,
		"participant_id":    0,
		"question_id":       questionID,
		"interaction_uuid":  newUUID(),
		"payload":           map[string]any{"user_input": prompt},
		"llm_settings": map[string]any{
			"model_name": s.cfg.Model, "temperature": 0.1, "max_tokens": 256, "stream": true,
		},
	}
}

func (s *suite) send(ctx context.Context, t *testing.T, body map[string]any) *client.Response {
	t.Helper()
	path := client.MessagesPath(s.projectID, s.conversationUUID) + "?execution_contract=" + client.AdhocContract
	return need(t)(s.main.api.Do(ctx, http.MethodPost, path, body, nil))
}

func (s *suite) chatIdempotency(t *testing.T) {
	// Generous: the first turn after elitea-main restarts (the seed step
	// restarts it) waits about a minute for the worker to pick it up.
	ctx := testContext(t, 5*time.Minute)
	s.requireSession(t)
	s.fatal = true
	s.projectID = s.personalProject(ctx, t)
	s.conversationID, s.conversationUUID = s.createConversation(ctx, t, "native conformance chat")

	questionID := newUUID()
	prompt := "native conformance " + strconv.FormatInt(time.Now().UnixNano(), 36)
	body := s.chatBody(prompt, questionID)

	first := s.send(ctx, t, body)
	if first.Status != http.StatusOK {
		t.Fatalf("chat send: %s", first)
	}
	var admitted client.ChatAdmission
	if err := first.JSON(&admitted); err != nil {
		t.Fatal(err)
	}
	if !admitted.Created || admitted.ExecutionID == "" || admitted.ResponseMessageID == "" {
		t.Fatalf("first send is not a fresh admission: %s", first)
	}
	wantEvents := fmt.Sprintf("/api/v2/executions/%d/%s/events", s.projectID, admitted.ExecutionID)
	if admitted.EventsURL != wantEvents {
		t.Errorf("events_url %q, want the same-origin absolute path %q", admitted.EventsURL, wantEvents)
	}
	// Connection 1 of the resume scenario, opened while the turn is live: a
	// few frames, then the network drops. stream_resume picks it up.
	s.head = s.readHead(ctx, t, admitted.EventsURL)

	// The identical request again: a replay, not a second turn.
	replay := s.send(ctx, t, body)
	if replay.Status != http.StatusOK {
		t.Fatalf("replayed send: %s", replay)
	}
	var replayed client.ChatAdmission
	if err := replay.JSON(&replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.Created || replayed.ExecutionID != admitted.ExecutionID || replayed.EventsURL != admitted.EventsURL ||
		replayed.ResponseMessageID != admitted.ResponseMessageID {
		t.Fatalf("a replay of the same question_id is not the original admission:\nfirst  %s\nreplay %s", first, replay)
	}

	// The same key with a different body is a conflict, never a second run.
	conflicting := s.chatBody(prompt+" (edited)", questionID)
	conflicting["interaction_uuid"] = body["interaction_uuid"]
	if response := s.send(ctx, t, conflicting); response.Status != http.StatusConflict {
		t.Errorf("same question_id, different body: %s, want 409", response)
	}
	// question_id is a LOWERCASE canonical UUID (coordinator decision 14).
	if response := s.send(ctx, t, s.chatBody(prompt, strings.ToUpper(newUUID()))); response.Status != http.StatusBadRequest {
		t.Errorf("an upper-case question_id: %s, want 400", response)
	}
	s.admission = admitted
	s.prompt = prompt
	s.fatal = false
	s.done(t)
}

// ── 5. The event stream resumes without gaps or duplicates ───────────────────

// readHead is the resume scenario's first connection: three frames of a live
// turn, then the client drops the connection.
func (s *suite) readHead(ctx context.Context, t *testing.T, events string) []client.Frame {
	t.Helper()
	stream, err := s.main.api.OpenStream(ctx, events, "")
	if err != nil {
		t.Fatal(err)
	}
	head, err := readFrames(ctx, stream, func(frames []client.Frame) bool {
		return len(frames) >= 3 || terminalSeen(frames)
	})
	_ = stream.Close()
	if err != nil {
		t.Fatalf("first stream connection: %v", err)
	}
	if terminalSeen(head) {
		t.Fatalf("the turn ended within %d frames; a resume needs a longer turn: %v", len(head), cursorsOf(head))
	}
	for index := 1; index < len(head); index++ {
		if head[index].Cursor <= head[index-1].Cursor {
			t.Fatalf("cursors are not strictly increasing: %v", cursorsOf(head))
		}
	}
	return head
}

// readFrames reads until stop says so, the context ends or the stream closes.
func readFrames(ctx context.Context, stream *client.Stream, stop func([]client.Frame) bool) ([]client.Frame, error) {
	type result struct {
		frame client.Frame
		err   error
	}
	var frames []client.Frame
	for {
		next := make(chan result, 1)
		go func() {
			event, err := stream.Next()
			if err != nil {
				next <- result{err: err}
				return
			}
			frame, err := client.DecodeFrame(event)
			next <- result{frame: frame, err: err}
		}()
		select {
		case <-ctx.Done():
			_ = stream.Close()
			return frames, fmt.Errorf("stream: %w after %d frames", ctx.Err(), len(frames))
		case got := <-next:
			if errors.Is(got.err, io.EOF) {
				return frames, client.ErrStreamEnded
			}
			if got.err != nil {
				return frames, got.err
			}
			frames = append(frames, got.frame)
			if stop(frames) {
				return frames, nil
			}
		}
	}
}

func terminalSeen(frames []client.Frame) bool {
	return len(frames) > 0 && client.IsTerminal(frames[len(frames)-1])
}

func cursorsOf(frames []client.Frame) []uint64 {
	out := make([]uint64, len(frames))
	for index, frame := range frames {
		out[index] = frame.Cursor
	}
	return out
}

func (s *suite) streamResume(t *testing.T) {
	ctx := testContext(t, 3*time.Minute)
	s.requireSession(t)
	if s.admission.EventsURL == "" {
		s.fatal = true
		t.Fatal("the chat scenario admitted no turn")
	}
	events := s.admission.EventsURL
	head := s.head
	if len(head) == 0 {
		t.Fatal("the chat scenario read no frames on its first connection")
	}
	last := head[len(head)-1].Cursor

	// Connection 2: resume with Last-Event-ID, read to the terminal frame.
	stream, err := s.main.api.OpenStream(ctx, events, strconv.FormatUint(last, 10))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := readFrames(ctx, stream, terminalSeen)
	_ = stream.Close()
	if err != nil {
		t.Fatalf("resumed connection: %v (cursors %v)", err, cursorsOf(tail))
	}
	joined := append(append([]client.Frame{}, head...), tail...)
	for index := 1; index < len(joined); index++ {
		if joined[index].Cursor <= joined[index-1].Cursor {
			t.Fatalf("cursors are not strictly increasing across the resume: %v", cursorsOf(joined))
		}
	}
	if client.IsFailure(tail[len(tail)-1]) {
		t.Fatalf("the turn failed: %v", tail[len(tail)-1].Data)
	}
	t.Logf("first connection %d frames (cursors %d..%d), resumed connection %d frames to %q",
		len(head), head[0].Cursor, last, len(tail), tail[len(tail)-1].Type)

	// ?cursor= is the same resume for clients that cannot set headers.
	stream, err = s.main.api.OpenStream(ctx, events+"?cursor="+strconv.FormatUint(last, 10), "")
	if err != nil {
		t.Fatal(err)
	}
	viaQuery, err := readFrames(ctx, stream, func(frames []client.Frame) bool { return len(frames) >= 1 })
	_ = stream.Close()
	if err != nil || viaQuery[0].Cursor != tail[0].Cursor {
		t.Errorf("?cursor= resume: first frame %v (err %v), want cursor %d", cursorsOf(viaQuery), err, tail[0].Cursor)
	}

	// A full replay is exactly head + tail: the resume dropped nothing.
	stream, err = s.main.api.OpenStream(ctx, events, "")
	if err != nil {
		t.Fatal(err)
	}
	full, err := readFrames(ctx, stream, terminalSeen)
	_ = stream.Close()
	if err != nil {
		t.Fatalf("full replay: %v", err)
	}
	if fmt.Sprint(cursorsOf(full)) != fmt.Sprint(cursorsOf(joined)) {
		t.Errorf("full replay %v differs from the resumed pair %v", cursorsOf(full), cursorsOf(joined))
	}

	// The deterministic mock echoes the prompt: the tokens came from THIS turn.
	var content strings.Builder
	for _, frame := range full {
		if frame.Type == "agent_llm_chunk" {
			chunk, _ := frame.Data["content"].(string)
			content.WriteString(chunk)
		}
	}
	if strings.Contains(s.cfg.Model, "E2E-MOCK") && !strings.Contains(content.String(), s.prompt) {
		t.Errorf("the streamed answer does not echo this turn's prompt; got %q", content.String())
	}

	s.awaitSettled(ctx, t)
	s.done(t)
}

// awaitSettled is the contract's robust settle check: the answer's group has
// no is_streaming and metadata.is_error is PRESENT (absent means running).
func (s *suite) awaitSettled(ctx context.Context, t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last *client.Response
	for time.Now().Before(deadline) {
		last = need(t)(s.main.api.Get(ctx, client.MessagesPath(s.projectID, s.conversationID)))
		if last.Status == http.StatusOK {
			body, _ := last.Map()
			items, _ := body["items"].([]any)
			for _, raw := range items {
				item, _ := raw.(map[string]any)
				if item["uid"] != s.admission.ResponseMessageID {
					continue
				}
				streaming, _ := item["is_streaming"].(bool)
				metadata, _ := item["metadata"].(map[string]any)
				isError, present := metadata["is_error"]
				if !streaming && present {
					if isError != false {
						t.Fatalf("the answer settled as an error: %v", item)
					}
					return
				}
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("the answer %s never settled; last read %s", s.admission.ResponseMessageID, last)
}

// ── 6. Conversation and message deltas ──────────────────────────────────────

func (s *suite) conversationSync(t *testing.T) {
	ctx := testContext(t, 2*time.Minute)
	s.requireSession(t)
	if s.conversationID == "" {
		t.Fatal("the chat scenario created no conversation")
	}
	list := client.ConversationPath(s.projectID)
	full, err := s.main.api.Sync(ctx, list, "0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.RowsByID(full.Rows)[s.conversationID]; !ok {
		t.Fatalf("a full sync (changes_since=0) misses conversation %s", s.conversationID)
	}

	createdID, _ := s.createConversation(ctx, t, "native conformance sync")
	delta, err := s.main.api.Sync(ctx, list, full.Cursor, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.RowsByID(delta.Rows)[createdID]; !ok {
		t.Fatalf("the delta after a create misses conversation %s (rows %d)", createdID, len(delta.Rows))
	}

	deleted := need(t)(s.main.api.Do(ctx, http.MethodDelete, client.ConversationItemPath(s.projectID, createdID), nil, nil))
	if deleted.Status != http.StatusOK && deleted.Status != http.StatusNoContent {
		t.Fatalf("delete conversation: %s", deleted)
	}
	afterDelete, err := s.main.api.Sync(ctx, list, delta.Cursor, 0)
	if err != nil {
		t.Fatal(err)
	}
	tombstone, ok := client.TombstoneFor(afterDelete.Tombstones, createdID)
	if !ok || tombstone.Reason != "deleted" || tombstone.DeletedAt == "" {
		t.Fatalf("the delta after a delete has no `deleted` tombstone for %s: %+v", createdID, afterDelete.Tombstones)
	}

	// Messages: the turn's groups arrive in a full sync, and a delta from
	// its cursor answers without error.
	messages := client.MessagesPath(s.projectID, s.conversationID)
	transcript, err := s.main.api.Sync(ctx, messages, "0", 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range transcript.Rows {
		if row["uid"] == s.admission.ResponseMessageID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the message delta misses the answer %s (%d rows)", s.admission.ResponseMessageID, len(transcript.Rows))
	}
	if _, err := s.main.api.Sync(ctx, messages, transcript.Cursor, 0); err != nil {
		t.Fatalf("message delta from its own cursor: %v", err)
	}
	// A cursor from another list is refused by name.
	_, err = s.main.api.Sync(ctx, list, transcript.Cursor, 0)
	var refused *client.SyncError
	if !errors.As(err, &refused) || refused.Response.Status != http.StatusBadRequest || refused.Code != "invalid_sync_cursor" {
		t.Errorf("a message cursor on the conversation list: %v, want 400 invalid_sync_cursor", err)
	}
	s.done(t)
}

// ── 7. Notification delta ───────────────────────────────────────────────────

func (s *suite) notificationSync(t *testing.T) {
	ctx := testContext(t, time.Minute)
	s.requireSession(t)
	if s.projectID == 0 {
		t.Fatal("no project resolved")
	}
	list := client.NotificationsPath(s.projectID)
	full, err := s.main.api.Sync(ctx, list, "0", 0)
	if err != nil {
		t.Fatal(err)
	}
	unseen := ""
	for _, row := range full.Rows {
		if row["is_seen"] == false {
			unseen = client.RowID(row)
			break
		}
	}
	if unseen == "" {
		t.Fatalf("no unseen notification in project %d to change; native-conformance.sh seed-native "+
			"inserts one per run (%d rows)", s.projectID, len(full.Rows))
	}
	marked := mustJSON(t, need(t)(s.main.api.Do(ctx, http.MethodPut, client.NotificationPath(s.projectID, unseen), nil, nil)),
		http.StatusOK)
	if marked["is_seen"] != true {
		t.Fatalf("mark seen answered %v", marked)
	}
	delta, err := s.main.api.Sync(ctx, list, full.Cursor, 0)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := client.RowsByID(delta.Rows)[unseen]
	if !ok || row["is_seen"] != true {
		t.Fatalf("the delta after marking %s seen does not carry it as seen: %v", unseen, row)
	}
	invalid := need(t)(s.main.api.Get(ctx, list+"?changes_since=0&offset=10"))
	if invalid.Status != http.StatusBadRequest || client.ErrorCode(invalid) != "invalid_sync_request" {
		t.Errorf("changes_since with offset: %s, want 400 invalid_sync_request", invalid)
	}
	s.done(t)
}

// ── 8. Minimum client version ───────────────────────────────────────────────

func (s *suite) minClientVersion(t *testing.T) {
	ctx := testContext(t, 2*time.Minute)
	s.requireSession(t)
	s.setMinimumVersion(t, "99.0.0")
	t.Cleanup(func() { s.setMinimumVersion(t, "") })

	result, err := client.New(s.cfg.Origin).FetchDiscovery(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Document.ClientPolicy.MinClientVersion != "99.0.0" ||
		result.Document.MinClientVersion[s.cfg.ClientID] != "99.0.0" {
		t.Errorf("discovery after raising the minimum: policy %q, per-client %v",
			result.Document.ClientPolicy.MinClientVersion, result.Document.MinClientVersion)
	}

	old := s.main.api.WithVersion("1.0.0")
	response := need(t)(old.Get(ctx, client.DevicesPath))
	if response.Status != http.StatusUpgradeRequired || response.Header.Get("X-Min-Client-Version") != "99.0.0" ||
		client.ErrorCode(response) != "client_upgrade_required" {
		t.Errorf("an outdated client on the API: %s (X-Min-Client-Version %q), want 426 client_upgrade_required",
			response, response.Header.Get("X-Min-Client-Version"))
	}
	if response := need(t)(s.main.api.WithVersion("not-a-version").Get(ctx, client.DevicesPath)); response.Status != http.StatusBadRequest {
		t.Errorf("a malformed X-Client-Version: %s, want 400", response)
	}
	if response := need(t)(s.main.api.WithVersion("99.0.0").Get(ctx, client.DevicesPath)); response.Status != http.StatusOK {
		t.Errorf("an up-to-date client: %s", response)
	}
	// The token endpoint answers 426 BEFORE it consumes the refresh token:
	// the same token works once the app is updated.
	_, err = old.Refresh(ctx, s.discovery.NativeAuth.TokenEndpoint, s.cfg.ClientID, s.main.tokens.RefreshToken)
	var refused *client.TokenError
	if !errors.As(err, &refused) || refused.Response.Status != http.StatusUpgradeRequired {
		t.Fatalf("an outdated client's refresh: %v, want 426", err)
	}
	s.main.api = s.main.api.WithVersion("99.0.0")
	s.main.refresh(ctx, t)
	// Cookie (and PAT) callers are exempt: only native tokens carry a client.
	if response := need(t)(s.admin.WithVersion("1.0.0").Get(ctx, adminClientsPath)); response.Status != http.StatusOK {
		t.Errorf("a console session stating an old version: %s, want it exempt", response)
	}

	s.setMinimumVersion(t, "")
	s.main.api = s.main.api.WithVersion("1.0.0")
	if response := need(t)(s.main.api.Get(ctx, client.DevicesPath)); response.Status != http.StatusOK {
		t.Errorf("after the minimum is cleared: %s", response)
	}
	s.done(t)
}

// ── 9. Device revocation ────────────────────────────────────────────────────

func (s *suite) deviceRevocation(t *testing.T) {
	ctx := testContext(t, 3*time.Minute)
	s.requireSession(t)

	// RFC 7009 from the app itself (sign out).
	signedOut := s.cfg.signIn(t, s.discovery, s.cfg.User, "oidc", "conformance sign-out")
	revoked := need(t)(signedOut.api.Revoke(ctx, s.discovery.NativeAuth.RevocationEndpoint, s.cfg.ClientID,
		signedOut.tokens.RefreshToken))
	if revoked.Status != http.StatusOK {
		t.Fatalf("revoke: %s", revoked)
	}
	if response := need(t)(signedOut.api.Get(ctx, client.DevicesPath)); !client.IsDeviceRevoked(response) {
		t.Errorf("an access token after its device signed out: %s, want 401 device_revoked", response)
	}

	// From the user's device list (another device, or this one).
	listing := mustJSON(t, need(t)(s.main.api.Get(ctx, client.DevicesPath)), http.StatusOK)
	row := deviceRow(listing, s.main.tokens.DeviceID)
	if row == nil || row["current"] != true {
		t.Fatalf("the device list does not show this device as current: %v", listing)
	}
	removed := need(t)(s.main.api.Do(ctx, http.MethodDelete, client.DevicesPath+"/"+s.main.tokens.DeviceID, nil, nil))
	if removed.Status != http.StatusOK && removed.Status != http.StatusNoContent {
		t.Fatalf("revoke this device: %s", removed)
	}
	response := need(t)(s.main.api.Get(ctx, client.DevicesPath))
	if !client.IsDeviceRevoked(response) {
		t.Fatalf("after revoking the device: %s, want 401 with the STRING error device_revoked", response)
	}
	if challenge := response.Header.Get("WWW-Authenticate"); !strings.Contains(challenge, "device_revoked") {
		t.Errorf("WWW-Authenticate %q does not name device_revoked", challenge)
	}
	_, err := s.main.api.Refresh(ctx, s.discovery.NativeAuth.TokenEndpoint, s.cfg.ClientID, s.main.tokens.RefreshToken)
	var refused *client.TokenError
	if !errors.As(err, &refused) || !client.IsDeviceRevoked(refused.Response) {
		t.Errorf("refresh after the device was revoked: %v, want 401 device_revoked", err)
	}
	s.done(t)
}
