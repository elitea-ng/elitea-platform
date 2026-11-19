package microsoftlogin

import (
	"encoding/json"
	"html"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftlogin"
)

// The callback page. It holds only static text and the data the web client needs: no
// string from Microsoft is ever rendered, so there is nothing to escape into the
// page, and the CSP allows its own script and style by hash and nothing else.

const (
	pageUnavailable = "Microsoft sign-in is not available on this deployment."
	pageInvalid     = "This sign-in link is not valid or has expired. Start the sign-in again from Elitea."
	pageDone        = "You are signed in. You can close this window."
)

const pageStyle = `body{font:16px/1.5 system-ui,sans-serif;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center;background:#f6f7f9;color:#1c2024}main{max-width:28rem;padding:2rem;text-align:center}@media (prefers-color-scheme:dark){body{background:#16181b;color:#e8eaed}}`

// pageScript reads the page's data and tells the web client. postMessage goes to the
// opener, targeted at the deployment origin; the BroadcastChannel reaches the same
// origin's windows even when an identity page severed window.opener.
const pageScript = `(function(){var d=JSON.parse(document.getElementById("d").textContent);var m={type:d.type,state:d.state};if(d.handoff){m.handoff=d.handoff}if(d.error){m.error=d.error}try{if(window.opener){window.opener.postMessage(m,d.origin)}}catch(e){}try{var c=new BroadcastChannel(d.channel);c.postMessage(m);c.close()}catch(e){}if(!d.error){try{window.close()}catch(e){}}})();`

var errorText = map[string]string{
	microsoftlogin.ErrDenied:     "The sign-in was cancelled.",
	microsoftlogin.ErrProvider:   "Microsoft did not complete the sign-in.",
	microsoftlogin.ErrExchange:   "Microsoft did not accept the sign-in. Try again.",
	microsoftlogin.ErrIdentity:   "Microsoft did not say who signed in.",
	microsoftlogin.ErrTenant:     "This account belongs to a different Microsoft tenant than the one this app is set up for.",
	microsoftlogin.ErrPermission: "The signed-in account lacks a permission this credential needs. Tick it in the credential and sign in again.",
	microsoftlogin.ErrRejected:   "Microsoft rejected the sign-in token. Try again.",
}

func writePage(w http.ResponseWriter, status int, script, message string) {
	csp := "default-src 'none'; style-src " + scriptHash(pageStyle) + "; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	if script != "" {
		csp = "default-src 'none'; script-src " + scriptHash(pageScript) + "; style-src " + scriptHash(pageStyle) +
			"; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", csp)
	w.WriteHeader(status)
	_, _ = w.Write([]byte("<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>Microsoft sign-in</title>" +
		"<style>" + pageStyle + "</style></head><body><main><p>" + html.EscapeString(message) + "</p></main>" + script +
		"</body></html>"))
}

type pageData struct {
	Type    string `json:"type"`
	State   string `json:"state"`
	Handoff string `json:"handoff,omitempty"`
	Error   string `json:"error,omitempty"`
	Origin  string `json:"origin"`
	Channel string `json:"channel"`
}

// writeWebPage answers the popup: the data is JSON in a non-executing script block,
// and the one script (allowed by hash) posts it.
func (h *Handler) writeWebPage(w http.ResponseWriter, outcome microsoftlogin.Outcome) {
	data, err := json.Marshal(pageData{
		Type: MessageType, State: outcome.State, Handoff: outcome.Handoff, Error: outcome.ErrorCode,
		Origin: h.origin, Channel: ChannelName,
	})
	if err != nil {
		writePage(w, http.StatusInternalServerError, "", pageUnavailable)
		return
	}
	message, status := pageDone, http.StatusOK
	if outcome.ErrorCode != "" {
		message = errorText[outcome.ErrorCode]
		if message == "" {
			message = "The sign-in could not be completed. Start it again from Elitea."
		}
	}
	// json.Marshal escapes <, > and & as \u00XX, so the data cannot close its block.
	script := "<script type=\"application/json\" id=\"d\">" + string(data) + "</script><script>" + pageScript + "</script>"
	writePage(w, status, script, message)
}
