package eliteacore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentmediaapi"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
)

// carriedAvatarPack is the avatar pack a fork or an import writes for one
// version entry of an export document into project `target` (ADR-0050
// decision 6, #1853). Both used to drop `avatar_pack` and copy
// `meta.icon_meta`, so the copy lost every state loop and went on showing
// the SOURCE project's still through icon_meta.
//
// The pack is checked as a version write checks it (agentmedia.NormalizePack),
// the export's embedded files (`avatar_media`) as uploads are, and every
// file the pack and icon_meta name is given its own object in the target's
// bucket, with the URLs rewritten to it (agentmediaapi.CarryMedia). The
// entry's `meta` map is rewritten in place, so call it before meta is
// encoded. A file that cannot be copied keeps its source URL and does not
// fail the copy; a pack or a file that fails its check is the version's
// error. The answer is the pack's JSON for a `::jsonb` parameter, or nil
// for no pack.
func (h *Handler) carriedAvatarPack(ctx context.Context, version map[string]any, target string) (any, error) {
	pack, err := agentmedia.NormalizePack(version["avatar_pack"])
	if err != nil {
		var refused *agentmedia.PackError
		if errors.As(err, &refused) {
			return nil, fmt.Errorf("avatar_pack (%s): %s", agentmedia.PackCode, refused.Message)
		}
		return nil, fmt.Errorf("avatar_pack: %w", err)
	}
	files, err := agentmediaapi.DecodeEmbeddedMedia(version[agentmediaapi.EmbeddedMediaKey])
	if err != nil {
		return nil, fmt.Errorf("%s (%s): %w", agentmediaapi.EmbeddedMediaKey, agentmedia.Code, err)
	}
	urls := agentmediaapi.PackURLs(pack)
	meta, _ := version["meta"].(map[string]any)
	if icon := agentmediaapi.IconMetaURL(meta); icon != "" {
		urls = append(urls, icon)
	}
	if len(urls) == 0 {
		return nil, nil
	}
	mapping := agentmediaapi.CarryMedia(ctx, h.store, target, urls, files)
	if meta != nil {
		agentmediaapi.RewriteIconMeta(meta, mapping)
	}
	if pack == nil {
		return nil, nil
	}
	rewritten, err := agentmediaapi.RewritePack(pack, mapping)
	if err != nil {
		return nil, fmt.Errorf("avatar_pack: %w", err)
	}
	return string(rewritten), nil
}

// exportedAvatarPack adds a version's pack to its export entry, with the
// exporting project's files embedded (`avatar_media`), so the file can be
// imported on another installation. Files of another project are not
// embedded: the pack keeps their URLs, and the import copies them when that
// project lives on the importing installation.
func (h *Handler) exportedAvatarPack(ctx context.Context, projectID string, entry map[string]any, packText *string, meta map[string]any) error {
	var urls []string
	if packText != nil {
		var pack map[string]any
		if err := json.Unmarshal([]byte(*packText), &pack); err != nil {
			return fmt.Errorf("decode avatar_pack: %w", err)
		}
		if pack != nil {
			entry["avatar_pack"] = pack
			urls = agentmediaapi.PackURLs(json.RawMessage(*packText))
		}
	}
	if icon := agentmediaapi.IconMetaURL(meta); icon != "" {
		urls = append(urls, icon)
	}
	if media := agentmediaapi.EmbedMedia(ctx, h.store, projectID, urls); media != nil {
		entry[agentmediaapi.EmbeddedMediaKey] = media
	}
	return nil
}
