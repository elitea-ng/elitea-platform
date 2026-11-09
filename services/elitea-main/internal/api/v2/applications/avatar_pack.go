package applications

import (
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

// applyAvatarPack reads `avatar_pack` from a version write body into v. An
// absent key leaves v alone; null clears the pack; an object is checked by
// agentmedia.NormalizePack and stored as its canonical JSON. Each pack URL
// must name an object of THIS project, so a version cannot point at another
// project's media.
//
// The still also goes into the meta patch as `icon_meta`, so a client that
// reads only meta.icon_meta (old web and mobile builds, the participant
// snapshot) shows the pack's still. Clearing the pack leaves icon_meta
// alone. Call it after v.Meta is assigned from the body. It writes the 400
// and answers false on a refusal.
func applyAvatarPack(w http.ResponseWriter, body map[string]any, projectID string, v *applications.Version) bool {
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
	if pack != nil && !packInProject(pack, projectID) {
		writeJSON(w, http.StatusBadRequest, avatarPackRefusal{
			Error: "every avatar pack url must name media uploaded to this project",
			Code:  agentmedia.PackCode,
		})
		return false
	}
	v.AvatarPack = pack
	v.Present.AvatarPack = true
	if still := agentmedia.StillURL(pack); still != "" {
		if v.Meta == nil {
			v.Meta = map[string]any{}
		}
		v.Meta["icon_meta"] = map[string]any{"url": still, "name": strings.TrimPrefix(still, agentmedia.URLPrefix)}
	}
	return true
}

// packInProject reports whether every url and poster of the pack names an
// object of projectID.
func packInProject(pack json.RawMessage, projectID string) bool {
	var decoded agentmedia.Pack
	if json.Unmarshal(pack, &decoded) != nil {
		return false
	}
	for _, slot := range decoded.Slots {
		for _, url := range []string{slot.URL, slot.PosterURL} {
			if url == "" {
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
