package applications

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
)

// avatarPackRefusal is the 400 body of a refused pack (ADR-0050 decision 6).
type avatarPackRefusal struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// storedAvatarPacks is what a pack write is checked against: the packs the
// application's versions already hold, and, on an update, the icon the
// version being replaced shows.
type storedAvatarPacks struct {
	packs []json.RawMessage
	// replacedStill is the still of the version an update replaces; "" on a
	// create.
	replacedStill string
	// replacedIcon is that version's meta.icon_meta url.
	replacedIcon string
}

// avatarPacksForUpdate reads the version an update replaces. A failed read
// is "nothing stored": the write is then checked as a new pack.
func (h *Handler) avatarPacksForUpdate(ctx context.Context, projectID, applicationID, versionID string) storedAvatarPacks {
	version, err := h.repo.GetVersion(ctx, projectID, applicationID, versionID)
	if err != nil {
		return storedAvatarPacks{}
	}
	icon, _ := iconMetaFromMeta(version.Meta)["url"].(string)
	stored := storedAvatarPacks{replacedStill: agentmedia.StillURL(version.AvatarPack), replacedIcon: icon}
	if len(version.AvatarPack) > 0 {
		stored.packs = []json.RawMessage{version.AvatarPack}
	}
	return stored
}

// avatarPacksForCreate reads every version of the application, so a Save
// As Version that forwards a stored pack is checked against it.
func (h *Handler) avatarPacksForCreate(ctx context.Context, projectID, applicationID string) storedAvatarPacks {
	versions, err := h.repo.ListVersions(ctx, projectID, applicationID)
	if err != nil {
		return storedAvatarPacks{}
	}
	var stored storedAvatarPacks
	for _, version := range versions {
		if len(version.AvatarPack) > 0 {
			stored.packs = append(stored.packs, version.AvatarPack)
		}
	}
	return stored
}

// applyAvatarPack reads `avatar_pack` from a version write body into v. An
// absent key leaves v alone; null clears the pack; an object is checked by
// agentmedia.NormalizePack and stored as its canonical JSON.
//
// Each pack URL must name an object of THIS project, or be a URL a pack of
// this application already holds: publish, the catalogue mirror and the MCP
// copy carry a pack into another project with its source URLs, and the
// editor forwards the stored pack on every save, so refusing those would
// make the copied version unsaveable. A version still cannot point at
// media of another project that it does not already show.
//
// The still goes into the meta patch as `icon_meta` only when it is new (no
// stored pack has it), so a client that reads only meta.icon_meta (old web
// and mobile builds, the participant snapshot) shows a changed still, while
// an icon picked on its own is not overwritten by an unchanged pack.
// Clearing the pack clears icon_meta when it still shows the removed still.
// Call it after v.Meta is assigned from the body. It writes the 400 and
// answers false on a refusal.
func applyAvatarPack(w http.ResponseWriter, body map[string]any, projectID string, v *applications.Version, stored storedAvatarPacks) bool {
	raw, present := body["avatar_pack"]
	if !present {
		return true
	}
	pack, err := agentmedia.NormalizePack(raw)
	if err != nil {
		var refused *agentmedia.PackError
		if errors.As(err, &refused) {
			writeJSON(w, http.StatusBadRequest, avatarPackRefusal{Error: refused.Message, Code: agentmedia.PackCode})
			return false
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the avatar pack could not be encoded"})
		return false
	}
	if pack != nil && !packInProject(pack, projectID, packURLs(stored.packs)) {
		writeJSON(w, http.StatusBadRequest, avatarPackRefusal{
			Error: "every avatar pack url must name media uploaded to this project",
			Code:  agentmedia.PackCode,
		})
		return false
	}
	v.AvatarPack = pack
	v.Present.AvatarPack = true
	still := agentmedia.StillURL(pack)
	switch {
	case still != "" && !stillHeld(still, stored.packs):
		if v.Meta == nil {
			v.Meta = map[string]any{}
		}
		v.Meta["icon_meta"] = map[string]any{"url": still, "name": strings.TrimPrefix(still, agentmedia.URLPrefix)}
	case pack == nil && stored.replacedStill != "":
		// The icon the body sends, else the stored one: clear it only when
		// it is the still being removed.
		icon := stored.replacedIcon
		if sent, ok := v.Meta["icon_meta"].(map[string]any); ok {
			icon, _ = sent["url"].(string)
		}
		if icon == stored.replacedStill {
			if v.Meta == nil {
				v.Meta = map[string]any{}
			}
			v.Meta["icon_meta"] = map[string]any{}
		}
	}
	return true
}

// packURLs lists every url and poster the packs hold.
func packURLs(packs []json.RawMessage) map[string]bool {
	urls := map[string]bool{}
	for _, raw := range packs {
		var decoded agentmedia.Pack
		if json.Unmarshal(raw, &decoded) != nil {
			continue
		}
		for _, slot := range decoded.Slots {
			for _, url := range []string{slot.URL, slot.PosterURL} {
				if url != "" {
					urls[url] = true
				}
			}
		}
	}
	return urls
}

// stillHeld reports whether a stored pack already has this still.
func stillHeld(still string, packs []json.RawMessage) bool {
	for _, raw := range packs {
		if agentmedia.StillURL(raw) == still {
			return true
		}
	}
	return false
}

// packInProject reports whether every url and poster of the pack names an
// object of projectID or one the application already holds.
func packInProject(pack json.RawMessage, projectID string, held map[string]bool) bool {
	var decoded agentmedia.Pack
	if json.Unmarshal(pack, &decoded) != nil {
		return false
	}
	for _, slot := range decoded.Slots {
		for _, url := range []string{slot.URL, slot.PosterURL} {
			if url == "" || held[url] {
				continue
			}
			if owner, _, _, ok := agentmedia.ParseURL(url); !ok || owner != projectID {
				return false
			}
		}
	}
	return true
}

// avatarPackValue is the wire value of a stored pack: the stored JSON as is,
// or nil (JSON null) for none. It reuses soulObject's rule.
func avatarPackValue(column *string) any { return soulValue(column) }
