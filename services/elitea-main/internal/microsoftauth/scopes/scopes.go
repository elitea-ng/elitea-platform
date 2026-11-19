// Package scopes holds the Microsoft Graph delegated permission lists of the
// Outlook, Teams and Microsoft Graph credentials, and the one normalizer per
// list (ADR-0057 decision 3).
//
// Ported from the SDK's configurations/microsoft_graph_scopes.py,
// configurations/{outlook,teams,ms_graph}.py and configurations/oauth_app_mode.py
// (EliteaAI/elitea-sdk, main). The credential save, the sign-in request and the
// connection check all call the SAME function for a list, so a stored value and
// a requested value cannot differ. Nothing is silently granted: a value a list
// does not offer is dropped and reported to the caller by name.
package scopes

import (
	"fmt"
	"regexp"
	"strings"
)

// GraphResourcePrefix is the resource a bare Graph permission name belongs to.
const GraphResourcePrefix = "https://graph.microsoft.com/"

// OfflineAccess is the OIDC scope that makes Microsoft return a refresh token.
const OfflineAccess = "offline_access"

// Option is one permission a fixed list offers, in display order.
type Option struct {
	Name        string
	Description string
}

// List is a fixed, ordered permission list with its default selection.
type List struct {
	options  []Option
	defaults []string
}

// Options returns the offered permissions in display order (a copy).
func (l List) Options() []Option { return append([]Option(nil), l.options...) }

// Names returns the offered permission names in display order.
func (l List) Names() []string {
	names := make([]string, len(l.options))
	for i, option := range l.options {
		names[i] = option.Name
	}
	return names
}

// Defaults returns the default selection (a copy).
func (l List) Defaults() []string { return append([]string(nil), l.defaults...) }

// Outlook is the Outlook permission list. Mail.Read is the default.
var Outlook = List{
	options: []Option{
		{"Mail.Read", "Read your mail: list, get, search and check new messages, read threads, list folders."},
		{"Mail.ReadWrite", "Change your mail: mark as read, move and delete messages. send_mail and reply_to_message also use it to return the sent message ID."},
		{"Mail.Send", "Send mail as you: send_mail and reply_to_message."},
		{"Mail.Read.Shared", "Read mail in mailboxes shared with you (when Mailbox is another person's address)."},
		{"Mail.ReadWrite.Shared", "Change mail in mailboxes shared with you (mark as read, move, delete)."},
		{"Mail.Send.Shared", "Send mail on behalf of a shared mailbox."},
	},
	defaults: []string{"Mail.Read"},
}

// Teams is the Teams permission list. ChannelMessage.Read.All needs Entra admin
// consent and is off by default.
var Teams = List{
	options: []Option{
		{"User.ReadBasic.All", "Look up people by email: chat members, senders and recipients."},
		{"Chat.Read", "Read your chats and chat messages: list_chats, find_chat_messages. search_teams_messages also needs ChannelMessage.Read.All."},
		{"ChatMessage.Send", "Send chat messages as you: send_chat_message."},
		{"Chat.Create", "Start a new 1:1 or group chat when send_chat_message finds no existing chat."},
		{"Team.ReadBasic.All", "List the teams you are a member of: list_teams, team names, connection test."},
		{"Channel.ReadBasic.All", "List the channels of a team: list_channels, channel names."},
		{"ChannelMessage.Send", "Post and reply in channels as you: send_channel_message."},
		{"ChannelMessage.Read.All", "Search and read channel messages (search_teams_messages across channels). Requires Entra admin consent. Leave off unless your Entra admin has granted it."},
	},
	defaults: []string{"User.ReadBasic.All", "Chat.Read", "Team.ReadBasic.All", "Channel.ReadBasic.All"},
}

// MSGraphDefault returns what an empty Microsoft Graph list means.
func MSGraphDefault() []string { return []string{"User.Read"} }

// GraphDefaultScope is the one scope that asks for everything the app was granted.
const GraphDefaultScope = GraphResourcePrefix + ".default"

var (
	splitPattern = regexp.MustCompile(`[,\s]+`)
	// permissionPattern is a Graph permission name: Mail.Read, Sites.ReadWrite.All.
	permissionPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*(\.[A-Za-z0-9_-]+)+$`)
	oidcScopes        = map[string]bool{"openid": true, "profile": true, "email": true, OfflineAccess: true}
)

// words flattens a stored value (a list, or a comma or space separated string)
// into its words.
func words(value any) []string {
	var items []string
	switch typed := value.(type) {
	case nil:
	case string:
		items = splitPattern.Split(typed, -1)
	case []string:
		for _, item := range typed {
			items = append(items, splitPattern.Split(item, -1)...)
		}
	case []any:
		for _, item := range typed {
			items = append(items, splitPattern.Split(fmt.Sprint(item), -1)...)
		}
	default:
		items = []string{fmt.Sprint(typed)}
	}
	return items
}

func stripGraphPrefix(text string) string {
	if len(text) >= len(GraphResourcePrefix) && strings.EqualFold(text[:len(GraphResourcePrefix)], GraphResourcePrefix) {
		return text[len(GraphResourcePrefix):]
	}
	return text
}

// Normalize keeps only the permissions the list offers, in the list's casing and
// order. It matches case-insensitively and strips the Graph resource prefix.
// dropped names every value the list does not offer, so the caller can warn; an
// empty selection becomes the list's default.
func (l List) Normalize(value any) (selected, dropped []string) {
	canonical := make(map[string]string, len(l.options))
	for _, option := range l.options {
		canonical[strings.ToLower(option.Name)] = option.Name
	}
	picked := map[string]bool{}
	for _, item := range words(value) {
		text := strings.TrimSpace(item)
		if text == "" {
			continue
		}
		if name, ok := canonical[strings.ToLower(stripGraphPrefix(text))]; ok {
			picked[name] = true
		} else {
			dropped = append(dropped, text)
		}
	}
	for _, option := range l.options {
		if picked[option.Name] {
			selected = append(selected, option.Name)
		}
	}
	if len(selected) == 0 {
		selected = l.Defaults()
	}
	return selected, dropped
}

// NormalizeMSGraph reads the free-text Microsoft Graph list. It strips the Graph
// prefix (keeping the full .default scope), drops OIDC scopes and duplicates
// (case-insensitive, first spelling wins), and refuses a value that is not a
// Graph permission, and .default mixed with anything else. Empty means User.Read.
func NormalizeMSGraph(value any) ([]string, error) {
	var result []string
	seen := map[string]bool{}
	for _, item := range words(value) {
		text := strings.TrimSpace(item)
		if text == "" || oidcScopes[strings.ToLower(text)] {
			continue
		}
		text = stripGraphPrefix(text)
		if strings.EqualFold(text, ".default") {
			text = GraphDefaultScope
		} else if !permissionPattern.MatchString(text) {
			return nil, fmt.Errorf("%q is not a Microsoft Graph permission. "+
				"Use names like Mail.Read or https://graph.microsoft.com/Mail.Read", item)
		}
		if key := strings.ToLower(text); !seen[key] {
			seen[key] = true
			result = append(result, text)
		}
	}
	if len(result) > 1 {
		for _, scope := range result {
			if scope == GraphDefaultScope {
				return nil, fmt.Errorf("%s cannot be combined with other permissions", GraphDefaultScope)
			}
		}
	}
	if len(result) == 0 {
		return MSGraphDefault(), nil
	}
	return result, nil
}

// DataverseScope is the one scope a Dataverse sign-in asks for: the
// environment's own user_impersonation. org is a validated https org URL.
func DataverseScope(org string) string { return strings.TrimRight(org, "/") + "/user_impersonation" }

// SignIn is what a sign-in asks Microsoft for: the ticked scopes plus
// offline_access, except when token refresh is off (autoRefresh is a non-nil
// false). A missing setting keeps offline_access: older credentials never
// stored the switch.
func SignIn(selected []string, autoRefresh *bool) []string {
	var wanted []string
	for _, scope := range selected {
		if scope != "" && scope != OfflineAccess {
			wanted = append(wanted, scope)
		}
	}
	if autoRefresh != nil && !*autoRefresh {
		return wanted
	}
	return append([]string{OfflineAccess}, wanted...)
}

// MissingPermissionHint is the 403 hint that names the permission a tool needs.
func MissingPermissionHint(needed string) string {
	if needed == "" {
		return " The signed-in account lacks the Microsoft Graph permission this tool needs: " +
			"tick it in the credential scopes and sign in again (an Entra admin may need to grant it)."
	}
	return " This tool needs the Microsoft Graph permission " + needed + ": tick it in the credential " +
		"scopes and sign in again (an Entra admin may need to grant it)."
}

// OutlookToolPermission names the permission each Outlook tool needs, for
// MissingPermissionHint. It is a function so the table stays immutable.
func OutlookToolPermission(tool string) string { return outlookTools[tool] }

// TeamsToolPermission names the permission each Teams tool needs.
func TeamsToolPermission(tool string) string { return teamsTools[tool] }

var outlookTools = map[string]string{
	"list_messages": "Mail.Read", "get_message": "Mail.Read", "search_messages": "Mail.Read",
	"check_new_messages": "Mail.Read", "find_new_messages": "Mail.Read",
	"get_thread_messages": "Mail.Read", "list_folders": "Mail.Read",
	"mark_as_read": "Mail.ReadWrite", "move_message": "Mail.ReadWrite", "delete_message": "Mail.ReadWrite",
	"send_mail": "Mail.Send", "reply_to_message": "Mail.Send",
}

var teamsTools = map[string]string{
	"list_teams": "Team.ReadBasic.All", "list_channels": "Channel.ReadBasic.All",
	"list_chats": "Chat.Read", "find_chat_messages": "Chat.Read",
	"search_teams_messages": "ChannelMessage.Read.All",
	"send_chat_message":     "ChatMessage.Send", "send_channel_message": "ChannelMessage.Send",
}
