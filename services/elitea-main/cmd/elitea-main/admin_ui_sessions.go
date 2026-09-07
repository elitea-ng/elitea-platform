package main

import (
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/adminui"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

// adminUISessions boxes the browser-session manager for the admin shell, or
// leaves the interface NIL when there is none.
//
// A nil *Manager assigned straight to an interface field yields a non-nil
// interface holding a nil pointer, and `h.cfg.Sessions != nil` downstream then
// reads as "configured" (#86). The admin shell would take the server-side
// branch for every cookie and refuse the legacy one, which on a deployment
// mid-upgrade is an empty sidebar for every operator.
func adminUISessions(manager *browsersession.Manager) adminui.BrowserSessions {
	if manager == nil {
		return nil
	}
	return manager
}
