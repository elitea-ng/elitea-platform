package localturns

// The desktop computer use screenshot upload (ADR-0047 decision 9, #1585):
// with the project's `computer_use.upload_screenshots` setting on (default
// off), the desktop host uploads each screenshot its computer tools take as
// an attachment of the turn's conversation, and records the answered path in
// the call's step record. Off, screenshots stay on the device and this route
// refuses them.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/desktopwire"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// ScreenshotPath is uploadLocalTurnScreenshot (client contract 1.41).
const ScreenshotPath = "/api/v2/elitea_core/local_turn_screenshot/prompt_lib/{projectID}/{executionID}"

// maxScreenshotBody bounds the JSON body: a screenshot (at most the image
// attachment limit, 3 MiB by default) as base64, and its fields.
const maxScreenshotBody = int64(6 * 1024 * 1024)

// The most one turn may upload: screenshots, and their bytes in all.
const (
	MaxTurnScreenshots     = 64
	MaxTurnScreenshotBytes = int64(64 * 1024 * 1024)
)

// ScreenshotUsage is what one turn has uploaded so far.
type ScreenshotUsage struct {
	Count int
	Bytes int64
}

// ErrScreenshotUploadOff is a project without the opt-in.
var ErrScreenshotUploadOff = errors.New("computer use screenshot upload is off for this project")

// ComputerUse is what the start and the screenshot upload read and write.
type ComputerUse interface {
	// UploadScreenshots reads the project's `computer_use` setting; a project
	// with no row has it off.
	UploadScreenshots(ctx context.Context, projectID int64) (bool, error)
	// ScreenshotTurn answers the conversation of the caller's started turn
	// when it may still take uploads: not committed, not expired and started
	// by credential. Otherwise localturn.ErrNotFound (another caller's turn,
	// or another credential's), localturn.ErrAlreadyCommitted or
	// localturn.ErrExpired.
	ScreenshotTurn(ctx context.Context, projectID, actorUserID int64, executionID string, credential localturn.Credential) (string, error)
	// StoreScreenshot stores content as an attachment of the conversation and
	// answers its filepath (`/{bucket}/{key}`). Errors are apierr errors.
	StoreScreenshot(ctx context.Context, projectID int64, conversationUUID, name, mediaType string, content []byte) (string, error)
	// ScreenshotUsage counts what the turn uploaded: the conversation's
	// screenshot attachments named after this turn's calls are this turn's.
	ScreenshotUsage(ctx context.Context, projectID int64, conversationUUID, executionID string) (ScreenshotUsage, error)
}

// WithComputerUse serves the start's `computer_use` and the screenshot
// upload. Without it, a start answers the upload off and the upload refuses.
func (route *Route) WithComputerUse(computer ComputerUse) *Route {
	if route != nil && route.h != nil && computer != nil {
		route.h.computer = computer
	}
	return route
}

// computerUseStarted is the start's `computer_use` object. An unreadable
// setting answers off: the screenshots then stay on the device.
func (h *handler) computerUseStarted(ctx context.Context, projectID int64) map[string]any {
	upload := false
	if h.computer != nil {
		on, err := h.computer.UploadScreenshots(ctx, projectID)
		if err != nil {
			slog.WarnContext(ctx, "local turn: computer use setting unreadable; screenshots stay on the device",
				"project_id", projectID, "err", err)
		}
		upload = on && err == nil
	}
	return map[string]any{"upload_screenshots": upload}
}

type screenshotBody struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Digest    string `json:"digest"`
	Content   string `json:"content"`
}

// screenshotName is the name the desktop host gives a screenshot.
var screenshotName = regexp.MustCompile(`^computer-screenshot-[A-Za-z0-9_-]{1,64}\.(png|jpg)$`)

func (body screenshotBody) bytes() ([]byte, bool) {
	match := screenshotName.FindStringSubmatch(body.Name)
	if match == nil {
		return nil, false
	}
	switch {
	case body.MediaType == "image/png" && match[1] == "png":
	case body.MediaType == "image/jpeg" && match[1] == "jpg":
	default:
		return nil, false
	}
	content, err := base64.StdEncoding.DecodeString(body.Content)
	if err != nil || len(content) == 0 {
		return nil, false
	}
	sum := sha256.Sum256(content)
	if !strings.EqualFold(body.Digest, "sha256:"+hex.EncodeToString(sum[:])) {
		return nil, false
	}
	return content, true
}

func (h *handler) uploadScreenshot(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := desktopwire.PositiveID(chi.URLParam(request, "projectID"))
	if !ok {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_local_turn", "Invalid local turn request")
		return
	}
	user, actor, ok := caller(writer, request)
	if !ok {
		return
	}
	var body screenshotBody
	if !decodeBody(writer, request, maxScreenshotBody, &body) {
		return
	}
	content, ok := body.bytes()
	if !ok {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_computer_screenshot",
			"The screenshot needs a computer-screenshot-<id>.png or .jpg name, its media type, base64 content and that content's sha256 digest.")
		return
	}
	ctx := request.Context()
	if h.computer == nil {
		writeScreenshotOff(writer)
		return
	}
	on, err := h.computer.UploadScreenshots(ctx, projectID)
	if err != nil {
		slog.ErrorContext(ctx, "local turn: computer use setting unreadable", "project_id", projectID, "err", err)
		writeUseCaseError(ctx, writer, "screenshot", localturn.ErrUnavailable)
		return
	}
	if !on {
		writeScreenshotOff(writer)
		return
	}
	conversationUUID, err := h.computer.ScreenshotTurn(ctx, projectID, actor, chi.URLParam(request, "executionID"),
		localturn.Credential{TokenID: user.TokenID, NativeClientID: user.NativeClientID})
	if err != nil {
		writeUseCaseError(ctx, writer, "screenshot", err)
		return
	}
	executionID := chi.URLParam(request, "executionID")
	usage, err := h.computer.ScreenshotUsage(ctx, projectID, conversationUUID, executionID)
	if err != nil {
		slog.ErrorContext(ctx, "local turn: count a turn's screenshots", "project_id", projectID, "err", err)
		writeUseCaseError(ctx, writer, "screenshot", localturn.ErrUnavailable)
		return
	}
	if usage.Count >= MaxTurnScreenshots {
		desktopwire.WriteError(writer, http.StatusTooManyRequests, "computer_screenshot_limit",
			"This turn uploaded the most screenshots one turn may (64); later ones stay on the device.")
		return
	}
	if usage.Bytes+int64(len(content)) > MaxTurnScreenshotBytes {
		desktopwire.WriteError(writer, http.StatusRequestEntityTooLarge, "computer_screenshot_limit",
			"This turn's screenshots would pass the 64 MiB one turn may upload; later ones stay on the device.")
		return
	}
	filepath, err := h.computer.StoreScreenshot(ctx, projectID, conversationUUID, turnScreenshotName(executionID, body.Name), body.MediaType, content)
	if err != nil {
		var refused *apierr.APIError
		if errors.As(err, &refused) && refused.Status < http.StatusInternalServerError {
			desktopwire.WriteError(writer, refused.Status, "computer_screenshot_refused", refused.Message)
			return
		}
		slog.ErrorContext(ctx, "local turn: store a computer screenshot", "project_id", projectID, "err", err)
		writer.Header().Set("Retry-After", "1")
		desktopwire.WriteError(writer, http.StatusServiceUnavailable, "local_turn_unavailable",
			"The screenshot could not be stored right now.")
		return
	}
	desktopwire.WriteJSON(writer, http.StatusCreated, map[string]any{"filepath": filepath, "file_size": len(content)})
}

// turnScreenshotName stores a screenshot under its turn's execution id, so
// the conversation's attachments say which turn uploaded it (and count it
// against the turn's cap) without a column of their own.
func turnScreenshotName(executionID, name string) string {
	return strings.Replace(name, "computer-screenshot-", TurnScreenshotPrefix(executionID), 1)
}

// TurnScreenshotPrefix is the name prefix of a turn's stored screenshots.
func TurnScreenshotPrefix(executionID string) string {
	return "computer-screenshot-" + executionID + "-"
}

func writeScreenshotOff(writer http.ResponseWriter) {
	desktopwire.WriteError(writer, http.StatusForbidden, "computer_screenshot_upload_off",
		"This project keeps computer use screenshots on the device (project setting computer_use.upload_screenshots is off).")
}
