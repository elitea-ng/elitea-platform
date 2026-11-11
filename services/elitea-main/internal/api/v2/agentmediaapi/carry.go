package agentmediaapi

// Carrying a pack's media into another project (#1853): fork, export and
// import.
//
// A pack names content-addressed files of ONE project's agent-media bucket
// (`/api/v2/agent_media/{project}/{sha256}.{ext}`). A fork or an import that
// carried the pack with those URLs would leave the copy showing another
// project's files: it works while that project keeps them, and stops the
// day the source deletes the agent, the project, or the orphan sweep reclaims
// a file no pack of its own still names. So the copy gets its OWN objects:
// the same bytes under the same name in the target project's bucket (the
// name is the digest, so nothing is renamed), and the URLs are rewritten to
// the target project.
//
// An export file leaves the platform, so it cannot point at a bucket: it
// embeds each file of the exporting project as base64 (`avatar_media`,
// keyed by file name). An import checks every embedded file as an upload is
// checked (agentmedia.Inspect, and the name must be the digest of the bytes
// with the right extension) before it stores it.
//
// Copying never fails the fork or the import it serves: a file that could
// not be copied keeps its source URL, which the public route still serves
// while the source has it. That is the behaviour publish and the catalogue
// mirror already have for every file, so the worst case is today's.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// EmbeddedMediaKey is the version key of an export that carries the pack's
// files.
const EmbeddedMediaKey = "avatar_media"

// PackURLs lists every media URL a stored pack names (slot URLs and video
// posters), each once.
func PackURLs(pack json.RawMessage) []string {
	var decoded agentmedia.Pack
	if len(pack) == 0 || json.Unmarshal(pack, &decoded) != nil {
		return nil
	}
	seen := map[string]bool{}
	var urls []string
	for _, name := range agentmedia.Slots {
		slot, ok := decoded.Slots[name]
		if !ok {
			continue
		}
		for _, url := range []string{slot.URL, slot.PosterURL} {
			if url != "" && !seen[url] {
				seen[url] = true
				urls = append(urls, url)
			}
		}
	}
	return urls
}

// RewritePack answers the pack with every URL in mapping replaced.
func RewritePack(pack json.RawMessage, mapping map[string]string) (json.RawMessage, error) {
	if len(pack) == 0 || len(mapping) == 0 {
		return pack, nil
	}
	var decoded agentmedia.Pack
	if err := json.Unmarshal(pack, &decoded); err != nil {
		return nil, err
	}
	for name, slot := range decoded.Slots {
		if to, ok := mapping[slot.URL]; ok {
			slot.URL = to
		}
		if to, ok := mapping[slot.PosterURL]; ok {
			slot.PosterURL = to
		}
		decoded.Slots[name] = slot
	}
	return json.Marshal(decoded)
}

// RewriteIconMeta points meta.icon_meta at the target's copy when its url
// is in mapping. The name follows the url when it was derived from it (the
// way a pack save writes it), so the icon reads as the copy everywhere.
func RewriteIconMeta(meta map[string]any, mapping map[string]string) {
	icon, ok := meta["icon_meta"].(map[string]any)
	if !ok {
		return
	}
	url, _ := icon["url"].(string)
	to, ok := mapping[url]
	if !ok {
		return
	}
	rewritten := make(map[string]any, len(icon))
	for key, value := range icon {
		rewritten[key] = value
	}
	rewritten["url"] = to
	if name, _ := icon["name"].(string); name == strings.TrimPrefix(url, agentmedia.URLPrefix) {
		rewritten["name"] = strings.TrimPrefix(to, agentmedia.URLPrefix)
	}
	meta["icon_meta"] = rewritten
}

// IconMetaURL is meta.icon_meta.url, or "".
func IconMetaURL(meta map[string]any) string {
	icon, _ := meta["icon_meta"].(map[string]any)
	url, _ := icon["url"].(string)
	return url
}

// EmbedMedia reads the files of `projectID` that urls name and answers them
// base64-encoded by file name, for an export. A URL of another project is
// left out (the exporting project does not own it); a file that cannot be
// read is left out and logged, so the export still succeeds and its URL
// stays in the pack.
func EmbedMedia(ctx context.Context, store storage.ObjectStore, projectID string, urls []string) map[string]string {
	if store == nil {
		return nil
	}
	embedded := map[string]string{}
	for _, url := range urls {
		owner, file, _, ok := agentmedia.ParseURL(url)
		if !ok || owner != projectID {
			continue
		}
		if _, done := embedded[file]; done {
			continue
		}
		data, err := readMedia(ctx, store, owner, file)
		if err != nil {
			slog.WarnContext(ctx, "agent media: export could not embed a file", "project_id", projectID, "file", file, "error", err)
			continue
		}
		embedded[file] = base64.StdEncoding.EncodeToString(data)
	}
	if len(embedded) == 0 {
		return nil
	}
	return embedded
}

// DecodeEmbeddedMedia checks an export's `avatar_media` map: every name must
// be `{sha256}.{ext}` of its own bytes, and the bytes must pass
// agentmedia.Inspect with that extension. nil is no media.
func DecodeEmbeddedMedia(raw any) (map[string][]byte, error) {
	if raw == nil {
		return nil, nil
	}
	entries, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New(EmbeddedMediaKey + " must be an object of base64 files")
	}
	files := make(map[string][]byte, len(entries))
	for file, value := range entries {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%s.%s must be a base64 string", EmbeddedMediaKey, file)
		}
		if base64.StdEncoding.DecodedLen(len(text)) > agentmedia.MaxBytes+3 {
			return nil, fmt.Errorf("%s.%s is larger than %d KiB", EmbeddedMediaKey, file, agentmedia.MaxBytes/1024)
		}
		data, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil, fmt.Errorf("%s.%s is not base64", EmbeddedMediaKey, file)
		}
		if err := checkFile(file, data); err != nil {
			return nil, fmt.Errorf("%s.%s: %w", EmbeddedMediaKey, file, err)
		}
		files[file] = data
	}
	return files, nil
}

// checkFile holds bytes to the name they are stored under.
func checkFile(file string, data []byte) error {
	_, name, extension, ok := agentmedia.ParseURL(agentmedia.URLOf("1", file))
	if !ok || name != file {
		return errors.New("the name must be {sha256}.{ext}")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:])+"."+extension != file {
		return errors.New("the name is not the SHA-256 of the bytes")
	}
	media, err := agentmedia.Inspect(data)
	if err != nil {
		return err
	}
	if media.Extension != extension {
		return fmt.Errorf("the bytes are %s, not .%s", media.Extension, extension)
	}
	return nil
}

// CarryMedia gives `target` its own copy of every file urls name, and
// answers the URL rewrites (source URL -> target URL) for the files that are
// now in the target's bucket. An embedded file is stored from its bytes; any
// other file is copied from its own project's bucket. A URL already of the
// target, a malformed URL, or a file that could not be stored is left out of
// the mapping, so its URL stays as it was (see the file comment). A nil store
// copies nothing.
func CarryMedia(ctx context.Context, store storage.ObjectStore, target string, urls []string, embedded map[string][]byte) map[string]string {
	mapping := map[string]string{}
	if store == nil {
		if len(urls) > 0 {
			slog.WarnContext(ctx, "agent media: no object store, the copy keeps the source media urls", "project_id", target)
		}
		return mapping
	}
	for _, url := range urls {
		owner, file, _, ok := agentmedia.ParseURL(url)
		if !ok {
			continue
		}
		if owner == target {
			// Already the target's URL. An export imported into a project of
			// the same id on ANOTHER installation still needs the bytes there.
			if data, ok := embedded[file]; ok {
				if err := copyInto(ctx, store, target, owner, file, data); err != nil {
					slog.WarnContext(ctx, "agent media: an embedded file was not stored", "project_id", target, "file", file, "error", err)
				}
			}
			continue
		}
		if err := copyInto(ctx, store, target, owner, file, embedded[file]); err != nil {
			slog.WarnContext(ctx, "agent media: the copy keeps a source media url",
				"project_id", target, "source_project_id", owner, "file", file, "error", err)
			continue
		}
		mapping[url] = agentmedia.URLOf(target, file)
	}
	return mapping
}

func copyInto(ctx context.Context, store storage.ObjectStore, target, owner, file string, data []byte) error {
	ref, err := storage.NewObjectRef(target, Bucket, file)
	if err != nil {
		return err
	}
	// Content-addressed: an object under this name already IS these bytes.
	if _, err := store.Stat(ctx, ref); err == nil {
		return nil
	}
	if data == nil {
		if data, err = readMedia(ctx, store, owner, file); err != nil {
			return err
		}
	}
	if err := checkFile(file, data); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	_, err = store.Put(ctx, ref, bytes.NewReader(data), storage.PutOptions{
		ContentType: agentmedia.MediaTypeOf(file[strings.LastIndexByte(file, '.')+1:]), ContentLength: int64(len(data)), ChecksumSHA256: sum[:],
	})
	return err
}

func readMedia(ctx context.Context, store storage.ObjectStore, projectID, file string) ([]byte, error) {
	ref, err := storage.NewObjectRef(projectID, Bucket, file)
	if err != nil {
		return nil, err
	}
	body, _, err := store.Get(ctx, ref, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, agentmedia.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > agentmedia.MaxBytes {
		return nil, fmt.Errorf("larger than %d bytes", agentmedia.MaxBytes)
	}
	return data, nil
}
