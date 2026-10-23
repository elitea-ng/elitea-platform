package eliteacore

// Skill icons — the port of legacy/plugins/elitea_core/api/v2/upload_skill_icon.py.
//
// MODELLED ON THE AGENT ICON FAMILY, deliberately, because two icon systems in
// one product is the failure this file exists to avoid. The agent routes
// (UploadIcon/DownloadIcon/DeleteIcon in handler.go) already settled every
// question this one would otherwise have to re-answer:
//
//   - bytes live in the reserved `icons` bucket of the caller's project
//     (iconBucket), not on a data dir;
//   - the public URL is /icons/{projectID}/{filename}, served by DownloadIcon
//     OUTSIDE the authenticated route group, because a browser <img src>
//     carries no Authorization header;
//   - the stored filename's extension is restricted to the image allowlist
//     (safeIconExtension), which is what closes the stored-XSS path through
//     that public route.
//
// WHY A FILENAME PREFIX AND NOT A KEY DIRECTORY. The download route's last
// segment is `{filename}` — ONE path segment. A key like `skill/abc.png` would
// need two, so it cannot be addressed through the route that already exists and
// is already public. Skill icons therefore share the agent icons' bucket and
// are distinguished by the `skill_` filename prefix, which is also what makes
// the listing possible: the GET lists the bucket filtered on that prefix, so a
// project's skill gallery never shows its agent icons and vice versa.
//
// WHAT THIS PORT DOES NOT DO. The legacy runtime decodes the upload through
// Pillow, rejects an image smaller than the requested box, and re-encodes every
// icon to PNG at at most 64x64. There is no image codec in this service and
// adding one is not in this change's scope, so the bytes are stored as uploaded
// and the declared `size` is the clamped box the client asked for. The
// consequence is honest and bounded: an icon may be served at its original
// pixel size, and the 512 KB cap below is the only size limit. It is NOT the
// legacy behaviour, and this comment is the disclosure — see the extension
// allowlist for the part that IS a security boundary and is enforced.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// quotedTenantSchema is tenantSchema's non-HTTP twin: the helpers below are
// called from two handlers with different failure shapes (a 400 for the PUT, a
// swallowed no-op for the upload's optional bind), so they return the error
// instead of writing a response.
func quotedTenantSchema(projectID string) (string, error) {
	quoted, err := tenantschema.Quote(projectID)
	if err != nil {
		return "", errInvalidTenant
	}
	return quoted, nil
}

// skillIconPrefix marks an object in the shared `icons` bucket as a SKILL
// icon. It is part of the stored filename, never a key directory — see the
// file header for why the download route forbids the latter.
const skillIconPrefix = "skill_"

// maxSkillIconBytes is the legacy MAX_FILE_SIZE_KB (512), in bytes.
const maxSkillIconBytes = 512 * 1024

// maxSkillIconDimension is the legacy MAX_ICON_DIMENSION.
const maxSkillIconDimension = 64

// defaultSkillIconPageSize is the legacy `limit` default for the listing.
const defaultSkillIconPageSize = 200

// skillIconListPageSize is the page size used to walk the object store. The
// store answers exactly one page per List call, so a project with more icons
// than this needs more than one call; the walk below is bounded by
// maxSkillIconListPages so a store that keeps reporting IsTruncated cannot
// spin this handler forever.
const skillIconListPageSize = 1000

const maxSkillIconListPages = 20

// safeIntOr is the legacy _safe_int: a client-supplied query or form value that
// is missing or non-numeric yields the default instead of a 500.
func safeIntOr(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return parsed
}

// clampIconDimension mirrors the legacy min(max(v, 1), 64): a zero or negative
// value would bypass the minimum-dimension guard, an oversized one inflates
// the thumbnail box.
func clampIconDimension(value int) int {
	if value < 1 {
		return 1
	}
	if value > maxSkillIconDimension {
		return maxSkillIconDimension
	}
	return value
}

// sizeofFmt is social/utils/image_utils.py:sizeof_fmt, whose output the
// icon_meta payload carries verbatim as `initial_file_size` /
// `resulting_file_size`.
func sizeofFmt(num int64) string {
	units := []string{"", "Ki", "Mi", "Gi", "Ti", "Pi", "Ei", "Zi"}
	value := float64(num)
	for _, unit := range units {
		if value < 1024.0 && value > -1024.0 {
			return fmt.Sprintf("%3.1f%sB", value, unit)
		}
		value /= 1024.0
	}
	return fmt.Sprintf("%.1fYiB", value)
}

func skillIconURL(projectID, filename string) string {
	return fmt.Sprintf("/icons/%s/%s", projectID, filename)
}

// ListSkillIcons answers GET /elitea_core/upload_skill_icon/prompt_lib/{projectID}.
//
// The shape is `{"rows": [...], "total": N}` — the shape
// social/rpc/icons.py:get_icons_list returns and the ONLY shape the client
// reads (skillsApi.js's getSkillIcons merges `rows` and reads `total`; the web
// client's unwrapListPage does the same). `items` would be a 200 that renders
// an empty gallery, which is the defect this family already shipped once.
//
// `total` is the count of every skill icon in the project, not the length of
// the page, exactly as get_icons_list computes it before slicing.
func (h *Handler) ListSkillIcons(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !validIconPathSegment(projectID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project"})
		return
	}
	if h.store == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "icon storage is not configured"})
		return
	}

	skip := safeIntOr(r.URL.Query().Get("skip"), 0)
	limit := safeIntOr(r.URL.Query().Get("limit"), defaultSkillIconPageSize)

	names, err := h.listSkillIconNames(r, projectID)
	if err != nil {
		slog.ErrorContext(r.Context(), "list skill icons", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to list icons"})
		return
	}

	rows := make([]map[string]any, 0, len(names))
	for _, name := range names {
		rows = append(rows, map[string]any{"name": name, "url": skillIconURL(projectID, name)})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total": len(rows),
		"rows":  pageOfIconRows(rows, skip, limit),
	})
}

// pageOfIconRows applies get_icons_list's `results[skip:skip + limit]` with
// Python's forgiving slice semantics: an out-of-range skip is an empty page,
// never a panic, and a non-positive limit means "everything from skip".
func pageOfIconRows(rows []map[string]any, skip, limit int) []map[string]any {
	if skip < 0 {
		skip = 0
	}
	if skip >= len(rows) {
		return []map[string]any{}
	}
	rest := rows[skip:]
	if limit > 0 && limit < len(rest) {
		rest = rest[:limit]
	}
	return rest
}

// listSkillIconNames returns every skill-icon filename in the project's icons
// bucket, sorted by name the way get_icons_list sorts its results.
func (h *Handler) listSkillIconNames(r *http.Request, projectID string) ([]string, error) {
	bucket, err := storage.NewBucketRef(projectID, iconBucket)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0)
	token := ""
	for page := 0; page < maxSkillIconListPages; page++ {
		listed, err := h.store.List(r.Context(), storage.ListQuery{
			Bucket:            bucket,
			KeyPrefix:         skillIconPrefix,
			MaxKeys:           skillIconListPageSize,
			ContinuationToken: token,
		})
		if err != nil {
			// A backend with no listing (storage.ErrNotSupported) is not a
			// server fault and must not 500 the gallery: it answers "no
			// uploaded icons", which is what such a deployment truly has to
			// show. Every other error is real and is reported.
			if errors.Is(err, storage.ErrNotSupported) {
				return names, nil
			}
			return nil, err
		}
		for _, object := range listed.Objects {
			// Defence in depth: the prefix is applied by the backend, but a
			// key carrying a separator could not be served by the single-
			// segment download route, so it is not a listable icon here.
			if !strings.HasPrefix(object.Key, skillIconPrefix) || strings.ContainsAny(object.Key, "/\\") {
				continue
			}
			names = append(names, object.Key)
		}
		if !listed.IsTruncated || listed.NextContinuationToken == "" {
			break
		}
		token = listed.NextContinuationToken
	}
	sort.Strings(names)
	return names, nil
}

// UploadSkillIcon answers POST
// /elitea_core/upload_skill_icon/prompt_lib/{projectID} and the same path with
// a trailing {versionId}. It stores the bytes and, when a version is named,
// binds the icon to that skill version's meta.icon_meta in one request — the
// two-call shape (upload then PUT) stays available and is what the picker uses
// for an already-uploaded icon.
//
// It returns the icon_meta object itself, as the legacy `return result['data']`
// does. The response is the payload the client PUTs back later, unchanged.
func (h *Handler) UploadSkillIcon(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !validIconPathSegment(projectID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project"})
		return
	}

	if err := r.ParseMultipartForm(maxSkillIconBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "No file in request.files"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		// The legacy runtime's own 400 for a request that names no file. Unlike the
		// agent route's "no file is a no-op", this family has a real listing to
		// keep honest: answering 200 for an upload that stored nothing is
		// exactly the success-without-persistence shape this port refuses.
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "No file in request.files"})
		return
	}
	defer func() { _ = file.Close() }()

	if header.Size > maxSkillIconBytes {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("File size exceeds %d KB", maxSkillIconBytes/1024),
		})
		return
	}
	if h.store == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "icon storage is not configured"})
		return
	}

	width := clampIconDimension(safeIntOr(r.FormValue("width"), maxSkillIconDimension))
	height := clampIconDimension(safeIntOr(r.FormValue("height"), maxSkillIconDimension))

	extension := safeIconExtension(header.Filename)
	filename := skillIconPrefix + generateID() + extension
	ref, err := storage.NewObjectRef(projectID, iconBucket, filename)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project"})
		return
	}

	// LimitReader, not a trusted header: header.Size is what the multipart
	// part declared, and the cap has to hold against a part that lies. The
	// extra byte is what makes an over-cap body detectable rather than
	// silently truncated into storage.
	limited := io.LimitReader(file, maxSkillIconBytes+1)
	info, err := h.store.Put(r.Context(), ref, limited, storage.PutOptions{
		ContentType:   mime.TypeByExtension(extension),
		ContentLength: -1,
	})
	if err != nil {
		slog.ErrorContext(r.Context(), "store skill icon", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to save file"})
		return
	}
	if info.Size > maxSkillIconBytes {
		// Stored, measured, over the cap: remove it rather than leave an
		// oversized object the listing would then advertise.
		if deleteErr := h.store.Delete(r.Context(), ref); deleteErr != nil {
			slog.ErrorContext(r.Context(), "remove oversized skill icon", "error", deleteErr)
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("File size exceeds %d KB", maxSkillIconBytes/1024),
		})
		return
	}

	iconMeta := map[string]any{
		"name":                filename,
		"url":                 skillIconURL(projectID, filename),
		"size":                fmt.Sprintf("%dx%d", width, height),
		"initial_file_size":   sizeofFmt(header.Size),
		"resulting_file_size": sizeofFmt(info.Size),
	}

	// The optional trailing path segment. The legacy runtime binds the icon to the
	// version in the same request when it is present, and the picker's "upload
	// then use" flow depends on it: without the bind the icon is stored but no
	// skill wears it.
	if versionID := chi.URLParam(r, "versionId"); versionID != "" {
		bound, err := h.bindSkillIcon(r, projectID, versionID, iconMeta)
		if err != nil {
			slog.ErrorContext(r.Context(), "bind skill icon", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to save icon"})
			return
		}
		if !bound {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"ok": false, "msg": fmt.Sprintf("There is no such version id %s", versionID),
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, iconMeta)
}

// UpdateSkillIcon answers PUT
// /elitea_core/upload_skill_icon/prompt_lib/{projectID}/{versionId}: it binds
// an already-uploaded icon to a skill version, or resets the version to the
// default icon when the payload's name and url are empty.
//
// It returns `{"updated": true}`, as the legacy runtime does, and 404 when the
// version does not exist. The 404 is the load-bearing half: a blind UPDATE that
// matched no row would answer "saved" for a skill that still has no icon.
func (h *Handler) UpdateSkillIcon(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	versionID := chi.URLParam(r, "versionId")

	var iconMeta map[string]any
	if err := json.NewDecoder(r.Body).Decode(&iconMeta); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	// The legacy runtime parses the body through UpdateIcon, whose `name` and
	// `url` are REQUIRED strings; a payload missing either is a 400 there, so it
	// is one here rather than an icon_meta the read path cannot render.
	if _, ok := iconMeta["name"].(string); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Validation error on item: name is required"})
		return
	}
	if _, ok := iconMeta["url"].(string); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Validation error on item: url is required"})
		return
	}

	bound, err := h.bindSkillIcon(r, projectID, versionID, iconMeta)
	if err != nil {
		if errors.Is(err, errNoTenantPool) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "database is not configured"})
			return
		}
		if errors.Is(err, errInvalidTenant) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
			return
		}
		slog.ErrorContext(r.Context(), "update skill icon", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to save icon"})
		return
	}
	if !bound {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "msg": fmt.Sprintf("There is no such version id %s", versionID),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

// DeleteSkillIcon answers DELETE
// /elitea_core/upload_skill_icon/prompt_lib/{projectID}/{name}: it unlinks the
// icon from every skill version wearing it and removes the object.
//
// The unlink runs FIRST and its failure is reported. Deleting the bytes while
// leaving the rows pointing at them is how a gallery ends up full of broken
// images with no way back.
func (h *Handler) DeleteSkillIcon(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	name := chi.URLParam(r, "name")
	if !validIconPathSegment(projectID) || !validIconPathSegment(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid icon"})
		return
	}

	if err := h.unlinkSkillIcon(r, projectID, name); err != nil {
		if errors.Is(err, errInvalidTenant) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid project id"})
			return
		}
		if !errors.Is(err, errNoTenantPool) {
			slog.ErrorContext(r.Context(), "unlink skill icon", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "failed to delete icon"})
			return
		}
	}

	if h.store != nil {
		ref, err := storage.NewObjectRef(projectID, iconBucket, name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid icon"})
			return
		}
		// Delete is documented idempotent (storage/errors.go), so a missing
		// object is not an error and the caller still gets `ok`.
		if err := h.store.Delete(r.Context(), ref); err != nil {
			slog.ErrorContext(r.Context(), "delete skill icon object", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "failed to delete icon"})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

var (
	errNoTenantPool  = errors.New("eliteacore: no database pool")
	errInvalidTenant = errors.New("eliteacore: invalid project id")
)

// bindSkillIcon writes icon_meta onto one skill version's meta, and reports
// whether a row was actually updated. The boolean is the whole point: the
// legacy runtime looks the version up and 404s when it is absent, and a caller
// that cannot be told the difference between "saved" and "matched nothing" is
// the success-without-persistence failure this family has already shipped once.
func (h *Handler) bindSkillIcon(r *http.Request, projectID, versionID string, iconMeta map[string]any) (bool, error) {
	if h.pool == nil {
		return false, errNoTenantPool
	}
	schema, err := quotedTenantSchema(projectID)
	if err != nil {
		return false, err
	}
	if _, err := strconv.Atoi(versionID); err != nil {
		// skill_versions.id is a SERIAL. A non-numeric segment cannot match a
		// row, and reaches the statement as a parameter regardless; refusing
		// it here keeps the answer a 404 rather than a driver type error.
		return false, nil
	}

	tag, err := h.pool.Exec(r.Context(), fmt.Sprintf(
		`UPDATE %s.skill_versions
		 SET meta = COALESCE(meta, '{}'::jsonb) || jsonb_build_object('icon_meta', $2::jsonb)
		 WHERE id = $1`, schema), versionID, mustJSON(iconMeta))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// unlinkSkillIcon clears icon_meta from every skill version that names this
// icon. Versions whose meta is NULL or carries no icon_meta are untouched.
func (h *Handler) unlinkSkillIcon(r *http.Request, projectID, name string) error {
	if h.pool == nil {
		return errNoTenantPool
	}
	schema, err := quotedTenantSchema(projectID)
	if err != nil {
		return err
	}
	_, err = h.pool.Exec(r.Context(), fmt.Sprintf(
		`UPDATE %s.skill_versions
		 SET meta = COALESCE(meta, '{}'::jsonb) || jsonb_build_object('icon_meta', '{}'::jsonb)
		 WHERE meta->'icon_meta'->>'name' = $1`, schema), name)
	return err
}
