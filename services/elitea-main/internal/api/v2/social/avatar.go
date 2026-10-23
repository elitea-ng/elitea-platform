package social

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentAvatarPath           = "/api/v2/social/avatar/{projectID}"
	CurrentAvatarDownloadPath   = "/avatars/{projectID}/{filename}"
	CurrentAvatarMode           = auth.PermissionModeDefault
	CurrentAvatarGetPermission  = "models.social.avatar.get"
	CurrentAvatarSetPermission  = "models.social.avatar.update"
	MaxCurrentAvatarUploadBytes = 5 << 20

	// avatarBucket is the reserved system bucket every uploaded avatar lands
	// in, mirroring eliteacore's iconBucket convention — no per-project quota
	// or retention requirement distinct from "keep it" is in scope here.
	avatarBucket = "avatars"
)

var ErrInvalidCurrentAvatarRoute = errors.New("invalid current avatar route dependencies")

// CurrentAvatarStore is the persistence boundary for the current user's own
// avatar URL, scoped by user_id (centry.social_users.avatar is per-user, not
// per-project — RBAC is still project-scoped like every other social route).
type CurrentAvatarStore interface {
	GetCurrentAvatar(ctx context.Context, userID int64) (*string, error)
	SetCurrentAvatar(ctx context.Context, userID int64, avatarURL string) error
}

// CurrentAvatarRoute owns the current-user avatar GET/PUT endpoints. It
// remains a standalone route until production composition explicitly mounts
// the complete Social slice, matching CurrentAuthorsRoute/
// CurrentFeedbackCreateRoute.
type CurrentAvatarRoute struct {
	handler http.Handler
}

func NewCurrentAvatarRoute(
	store CurrentAvatarStore,
	objectStore storage.ObjectStore,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentAvatarRoute, error) {
	if store == nil || !authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentAvatarRoute
	}

	h := &currentAvatarHandler{store: store, objectStore: objectStore}

	getEndpoint := http.Handler(http.HandlerFunc(h.get))
	getEndpoint = apimw.RequireResolvedPermissionsForProject(
		permissions, CurrentAvatarMode, currentAvatarProjectID, CurrentAvatarGetPermission,
	)(getEndpoint)
	getEndpoint = apimw.Auth(authConfig)(getEndpoint)

	putEndpoint := http.Handler(http.HandlerFunc(h.upload))
	putEndpoint = apimw.RequireResolvedPermissionsForProject(
		permissions, CurrentAvatarMode, currentAvatarProjectID, CurrentAvatarSetPermission,
	)(putEndpoint)
	putEndpoint = apimw.Auth(authConfig)(putEndpoint)

	router := chi.NewRouter()
	router.Method(http.MethodGet, CurrentAvatarPath, getEndpoint)
	router.Method(http.MethodPut, CurrentAvatarPath, putEndpoint)
	return &CurrentAvatarRoute{handler: router}, nil
}

func (route *CurrentAvatarRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentAvatarHandler struct {
	store       CurrentAvatarStore
	objectStore storage.ObjectStore
}

type currentAvatarResponse struct {
	Avatar *string `json:"avatar"`
}

func (h *currentAvatarHandler) get(w http.ResponseWriter, r *http.Request) {
	userID, ok := currentAvatarUserID(r)
	if !ok {
		writeCurrentAvatarError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	avatar, err := h.store.GetCurrentAvatar(r.Context(), userID)
	if err != nil {
		writeCurrentAvatarError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, currentAvatarResponse{Avatar: avatar})
}

func (h *currentAvatarHandler) upload(w http.ResponseWriter, r *http.Request) {
	userID, ok := currentAvatarUserID(r)
	if !ok {
		writeCurrentAvatarError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	projectID := chi.URLParam(r, "projectID")

	if h.objectStore == nil {
		writeCurrentAvatarError(w, http.StatusInternalServerError, "avatar storage is not configured")
		return
	}
	if !validAvatarPathSegment(projectID) {
		writeCurrentAvatarError(w, http.StatusBadRequest, "invalid project")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxCurrentAvatarUploadBytes)
	if err := r.ParseMultipartForm(MaxCurrentAvatarUploadBytes); err != nil {
		writeCurrentAvatarError(w, http.StatusBadRequest, "invalid multipart request")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	file, header, err := r.FormFile("file")
	if err != nil {
		writeCurrentAvatarError(w, http.StatusBadRequest, "file is required")
		return
	}
	defer func() { _ = file.Close() }()

	extension := safeAvatarExtension(header.Filename)
	filename := generateAvatarID() + extension
	ref, err := storage.NewObjectRef(projectID, avatarBucket, filename)
	if err != nil {
		writeCurrentAvatarError(w, http.StatusBadRequest, "invalid project")
		return
	}

	contentType := mime.TypeByExtension(extension)
	if _, err := h.objectStore.Put(r.Context(), ref, file, storage.PutOptions{
		ContentType: contentType, ContentLength: -1,
	}); err != nil {
		writeCurrentAvatarError(w, http.StatusInternalServerError, "failed to save file")
		return
	}

	url := "/avatars/" + projectID + "/" + filename
	if err := h.store.SetCurrentAvatar(r.Context(), userID, url); err != nil {
		// The object already landed in storage; without this delete it would
		// be orphaned (never referenced by any row, never cleaned up).
		_ = h.objectStore.Delete(r.Context(), ref)
		writeCurrentAvatarError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, currentAvatarResponse{Avatar: &url})
}

// DownloadAvatar serves an uploaded avatar at the exact URL upload already
// returns (/avatars/{projectID}/{filename}). Deliberately not gated by Auth:
// a browser <img src="..."> carries no Authorization header, matching
// eliteacore.DownloadIcon's placement outside the authenticated route group.
func DownloadAvatar(store storage.ObjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		projectID := chi.URLParam(r, "projectID")
		filename := chi.URLParam(r, "filename")
		ref, err := storage.NewObjectRef(projectID, avatarBucket, filename)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _, err := store.Get(r.Context(), ref, nil)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		defer func() { _ = body.Close() }()

		// nosniff + a content type derived only from the allowlisted
		// extension (never the backend-reported ObjectInfo.ContentType)
		// closes the same stored-XSS path eliteacore.DownloadIcon guards
		// against on this equally public, unauthenticated route.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if contentType := mime.TypeByExtension(safeAvatarExtension(filename)); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, body)
	}
}

func currentAvatarUserID(r *http.Request) (int64, bool) {
	principal, ok := auth.UserFromContext(r.Context())
	if !ok {
		return 0, false
	}
	return principal.OwningUserID()
}

func currentAvatarProjectID(request *http.Request) (string, bool) {
	value := chi.URLParam(request, "projectID")
	projectID, err := strconv.ParseInt(value, 10, 64)
	return value, err == nil && projectID > 0 && strconv.FormatInt(projectID, 10) == value
}

func writeCurrentAvatarError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func generateAvatarID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error on supported platforms
	return hex.EncodeToString(b)
}

func validAvatarPathSegment(value string) bool {
	return value != "" &&
		value != "." &&
		value != ".." &&
		len(value) <= 255 &&
		!strings.ContainsAny(value, "/\\\x00")
}

var allowedAvatarExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

func safeAvatarExtension(filename string) string {
	const defaultExtension = ".png"

	if lastSeparator := strings.LastIndexAny(filename, "/\\"); lastSeparator >= 0 {
		filename = filename[lastSeparator+1:]
	}
	lastDot := strings.LastIndexByte(filename, '.')
	if lastDot <= 0 || lastDot == len(filename)-1 {
		return defaultExtension
	}
	extension := strings.ToLower(filename[lastDot:])
	if len(extension) > 16 {
		return defaultExtension
	}
	for _, char := range extension[1:] {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return defaultExtension
		}
	}
	if !allowedAvatarExtensions[extension] {
		return defaultExtension
	}
	return extension
}
