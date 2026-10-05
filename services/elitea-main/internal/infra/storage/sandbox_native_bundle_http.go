package storage

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/go-chi/chi/v5"
	"google.golang.org/protobuf/proto"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
)

// Called only after authorize verified this same header, TLS peer and exact root.
func nativePreparationGrant(r *http.Request, b *NativeSandboxBundle) bool {
	raw, err := base64.StdEncoding.Strict().DecodeString(r.Header.Get(SandboxBundleGrantHeader))
	if err != nil {
		return false
	}
	var g runtimev1.SignedSandboxJobGrantV1
	var c runtimev1.SandboxJobGrantClaimsV1
	if proto.Unmarshal(raw, &g) != nil || proto.Unmarshal(g.ClaimsBytes, &c) != nil {
		return false
	}
	return hex.EncodeToString(c.RequestDigest) == b.record.Preparation
}
func (service *SandboxBundleContentService) NativeRoutes() http.Handler {
	router := chi.NewRouter()
	router.Get("/{root}", service.nativeMetadata)
	router.Post("/{root}", service.nativePublish)
	router.Put("/{root}/files/{name}", service.nativeUpload)
	router.Get("/{root}/files/{name}", service.nativeDownload)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setPrivateNoCacheHeaders(w.Header())
		select {
		case service.requests <- struct{}{}:
			defer func() { <-service.requests }()
		default:
			sandboxBundleHTTPError(w, r, ErrContentUnavailable)
			return
		}
		router.ServeHTTP(w, r)
	})
}

func (service *SandboxBundleContentService) nativePublish(w http.ResponseWriter, r *http.Request) {
	scope, ok := service.scope(w, r)
	if !ok {
		return
	}
	native, err := sandboxBundleJSONBody(w, r)
	if err == nil {
		var bundle *NativeSandboxBundle
		bundle, err = ParseNativeSandboxBundle(native, chi.URLParam(r, "root"))
		if err == nil {
			if !nativePreparationGrant(r, bundle) {
				err = ErrContentUnauthorized
			} else {
				err = service.store.Publish(r.Context(), scope, bundle)
			}
		}
	}
	if err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (service *SandboxBundleContentService) nativeUpload(w http.ResponseWriter, r *http.Request) {
	scope, ok := service.scope(w, r)
	if !ok {
		return
	}
	const maximum = sandboxBundleMetadataLimit + 128*1024*1024 + 4096
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || len(params) != 1 || params["boundary"] == "" || len(params["boundary"]) > 70 || len(r.TransferEncoding) != 0 || r.ContentLength <= 0 || r.ContentLength > maximum {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maximum)
	parts := multipart.NewReader(r.Body, params["boundary"])
	metadata, err := parts.NextRawPart()
	if err != nil || metadata.FormName() != "bundle" || metadata.FileName() != "" || metadata.Header.Get("Content-Type") != "application/json" || metadata.Header.Get("Content-Transfer-Encoding") != "" {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	native, err := io.ReadAll(io.LimitReader(metadata, sandboxBundleMetadataLimit+1))
	if err != nil || len(native) > sandboxBundleMetadataLimit {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	bundle, err := ParseNativeSandboxBundle(native, chi.URLParam(r, "root"))
	if err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	name := chi.URLParam(r, "name")
	if _, err := bundle.file(name); err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	if !nativePreparationGrant(r, bundle) {
		sandboxBundleHTTPError(w, r, ErrContentUnauthorized)
		return
	}
	file, err := parts.NextRawPart()
	if err != nil || file.FormName() != "file" || file.FileName() != name || file.Header.Get("Content-Type") != "application/octet-stream" || file.Header.Get("Content-Transfer-Encoding") != "" {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	if err := service.store.PutFile(r.Context(), scope, bundle, name, file); err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	// Any already-written bytes are verified content under this exact root.
	// Reject an extra part; do not publish completion for malformed requests.
	if _, err := parts.NextRawPart(); err != io.EOF {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (service *SandboxBundleContentService) nativeMetadata(w http.ResponseWriter, r *http.Request) {
	scope, ok := service.scope(w, r)
	if !ok {
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	bundle, err := service.store.OpenNativeBundle(r.Context(), scope, chi.URLParam(r, "root"))
	if err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(bundle.native)))
	if _, err := w.Write(bundle.native); err != nil {
		slog.ErrorContext(r.Context(), "sandbox bundle metadata delivery failed")
	}
}

func (service *SandboxBundleContentService) nativeDownload(w http.ResponseWriter, r *http.Request) {
	scope, ok := service.scope(w, r)
	if !ok {
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		sandboxBundleHTTPError(w, r, ErrContentRejected)
		return
	}
	bundle, err := service.store.OpenNativeBundle(r.Context(), scope, chi.URLParam(r, "root"))
	if err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	file, err := bundle.file(chi.URLParam(r, "name"))
	if err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	staged, err := os.CreateTemp(service.store.spoolDir, "delivery-*")
	if err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	defer func() {
		if err := errors.Join(staged.Close(), os.Remove(staged.Name())); err != nil {
			slog.ErrorContext(r.Context(), "sandbox bundle delivery staging cleanup failed")
		}
	}()
	// Validate before sending headers. Corrupt storage cannot yield HTTP success.
	if err := service.store.FetchFile(r.Context(), scope, bundle, file.Name, staged); err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		sandboxBundleHTTPError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(file.Bytes, 10))
	hash, _ := hex.DecodeString(file.SHA256)
	w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(hash)+":")
	if _, err := io.Copy(w, sandboxContextReader{r.Context(), staged}); err != nil {
		slog.ErrorContext(r.Context(), "sandbox bundle file delivery failed")
	}
}
