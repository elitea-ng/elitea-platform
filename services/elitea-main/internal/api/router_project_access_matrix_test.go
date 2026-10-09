package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// ---------------------------------------------------------------------------
// Object-level access to the project-scoped reads and writes whose route used
// to carry no gate of its own (or relied on a repository that never asked):
//
//	GET          /elitea_core/feedbacks/default/{projectID}
//	GET          /elitea_core/platform_settings/prompt_lib/{projectID}
//	POST/DELETE  /elitea_core/pin/prompt_lib/{projectID}/{type}/{id}
//
// and their social twins, which were already gated and are held to the same
// table so the two families cannot drift apart again.
//
// The membership answer is keyed on the (project, user) pair — unlike
// memberOfProject, which keys on the project alone — because the claim under
// test is about WHO is asking, not only what they ask for.
//
// "The tenant table was not touched" is measured, not inferred: cfg.Pool is a
// pool whose server records the text of every statement it is sent and refuses
// it. A refused request must leave no statement that names the project's
// tenant schema or the pin, feedback, social or auth tables; an admitted one
// must send the statement its handler owns (the positive control that keeps the
// empty result meaningful).
// ---------------------------------------------------------------------------

// projectMembers answers the middleware's membership query for explicit
// (project, user) pairs and records every pair it was asked about.
type projectMembers struct {
	members map[[2]int]bool
	asked   [][2]int
}

func (m *projectMembers) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	project, _ := args[0].(int)
	user, _ := args[1].(int)
	m.asked = append(m.asked, [2]int{project, user})
	return membershipRow{allowed: m.members[[2]int{project, user}]}
}

// tokenTable accepts a fixed set of bearer tokens, each naming one principal.
type tokenTable map[string]auth.User

func (t tokenTable) ValidateToken(_ context.Context, token string) (auth.User, error) {
	user, ok := t[token]
	if !ok {
		return auth.User{}, fmt.Errorf("test validator: unexpected token %q", token)
	}
	return user, nil
}

// recordingPostgres is a minimal PostgreSQL server that records the text of
// every statement a client sends and answers each with an error. A pgx pool
// pointed at it lets a test state precisely which tables a request tried to
// read or write — the router's own housekeeping (the maintenance read) shows up
// too, which is why the assertions name tables instead of counting dials.
type recordingPostgres struct {
	mu         sync.Mutex
	statements []string
}

func (p *recordingPostgres) record(sql string) {
	p.mu.Lock()
	p.statements = append(p.statements, sql)
	p.mu.Unlock()
}

// sent returns the statements that name any of the fragments.
func (p *recordingPostgres) sent(fragments ...string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var hits []string
	for _, statement := range p.statements {
		for _, fragment := range fragments {
			if strings.Contains(statement, fragment) {
				hits = append(hits, statement)
				break
			}
		}
	}
	return hits
}

func (p *recordingPostgres) reset() {
	p.mu.Lock()
	p.statements = nil
	p.mu.Unlock()
}

func newRecordingPostgresPool(t *testing.T) (*pgxpool.Pool, *recordingPostgres) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &recordingPostgres{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.serve(conn)
		}
	}()
	pool, err := pgxpool.New(context.Background(), fmt.Sprintf(
		"postgres://nobody@%s/none?sslmode=disable&connect_timeout=2", listener.Addr()))
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_ = listener.Close()
	})
	return pool, server
}

func pgMessage(kind byte, payload ...byte) []byte {
	out := make([]byte, 0, 5+len(payload))
	out = append(out, kind)
	out = binary.BigEndian.AppendUint32(out, uint32(4+len(payload)))
	return append(out, payload...)
}

func pgReady() []byte { return pgMessage('Z', 'I') }

func pgError() []byte {
	return pgMessage('E', []byte("SERROR\x00C57014\x00Mrecording server refuses every statement\x00\x00")...)
}

func (p *recordingPostgres) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	// Startup message: no type byte.
	var length [4]byte
	if _, err := io.ReadFull(reader, length[:]); err != nil {
		return
	}
	if _, err := io.CopyN(io.Discard, reader, int64(binary.BigEndian.Uint32(length[:]))-4); err != nil {
		return
	}
	hello := pgMessage('R', 0, 0, 0, 0)
	hello = append(hello, pgMessage('S', []byte("server_version\x0016.0\x00")...)...)
	hello = append(hello, pgMessage('S', []byte("client_encoding\x00UTF8\x00")...)...)
	hello = append(hello, pgMessage('K', 0, 0, 0, 1, 0, 0, 0, 2)...)
	hello = append(hello, pgReady()...)
	if _, err := conn.Write(hello); err != nil {
		return
	}
	for {
		kind, err := reader.ReadByte()
		if err != nil {
			return
		}
		if _, err := io.ReadFull(reader, length[:]); err != nil {
			return
		}
		payload := make([]byte, binary.BigEndian.Uint32(length[:])-4)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return
		}
		var reply []byte
		switch kind {
		case 'X':
			return
		case 'Q':
			sql := strings.TrimRight(string(payload), "\x00")
			p.record(sql)
			switch strings.ToLower(strings.TrimSpace(sql)) {
			case "begin", "commit", "rollback":
				reply = append(pgMessage('C', append([]byte(strings.ToUpper(strings.TrimSpace(sql))), 0)...), pgReady()...)
			default:
				reply = append(pgError(), pgReady()...)
			}
		case 'P':
			// name\0 query\0 ...
			rest := payload[bytes.IndexByte(payload, 0)+1:]
			p.record(string(rest[:bytes.IndexByte(rest, 0)]))
			reply = pgError()
		case 'S':
			reply = pgReady()
		}
		if reply != nil {
			if _, err := conn.Write(reply); err != nil {
				return
			}
		}
	}
}

const accessMatrixSecret = "access-matrix-session-secret"

// Project 7 is the target. Users: 1 owns it, 2 is a member, 3 belongs to
// project 8 only (a foreign-project actor), 4 belongs to nothing.
func accessMatrixMembers() *projectMembers {
	return &projectMembers{members: map[[2]int]bool{
		{7, 1}: true, {7, 2}: true, {8, 3}: true,
	}}
}

func accessMatrixTokens() tokenTable {
	pat := func(id string) auth.User {
		return auth.User{ID: id, UserID: id, TokenID: "t" + id, AuthType: "token", Email: "u" + id + "@test.local"}
	}
	return tokenTable{
		"pat-1": pat("1"), "pat-2": pat("2"), "pat-3": pat("3"), "pat-4": pat("4"),
		// A token that names no owning user.
		"pat-orphan": {ID: "9", TokenID: "t9", AuthType: "token"},
	}
}

func newAccessMatrixRouter(t *testing.T, members *projectMembers) (chi.Router, *recordingPostgres) {
	t.Helper()
	pool, db := newRecordingPostgresPool(t)
	router := NewRouter(RouterConfig{
		Pool:                 pool,
		AuthValidator:        accessMatrixTokens(),
		PrincipalValidator:   testPrincipalValidator{},
		SessionSecret:        accessMatrixSecret,
		ProjectAccessQuerier: members,
	})
	// Anything the router did while being built is not the request's.
	db.reset()
	return router, db
}

// credential presents one actor either as a personal access token or as a
// browser session cookie.
type credential struct {
	kind string
	user int
}

func (c credential) apply(r *http.Request) {
	switch c.kind {
	case "pat":
		r.Header.Set("Authorization", fmt.Sprintf("Bearer pat-%d", c.user))
	case "session":
		r.AddCookie(&http.Cookie{Name: "elitea_session", Value: sessionToken(accessMatrixSecret, fmt.Sprint(c.user))})
	}
}

// protectedFragments name what a refused request must never send to the
// database: the target project's tenant schema, and the shared tables the
// handlers and the pin repository read and write.
var protectedFragments = []string{
	"p_7", "social_pins", "social_feedbacks", "social_likes", "social_users", "auth_core__",
}

type accessRoute struct {
	method string
	// path has one %d: the project id.
	path string
	// touches names a fragment of the statement an ADMITTED request sends: the
	// positive control.
	touches string
}

var accessMatrixRoutes = []accessRoute{
	{http.MethodGet, "/api/v2/elitea_core/feedbacks/default/%d", "social_feedbacks"},
	{http.MethodGet, "/api/v2/social/feedbacks/default/%d", "social_feedbacks"},
	{http.MethodGet, "/api/v2/elitea_core/platform_settings/prompt_lib/%d", "environment_settings"},
	{http.MethodPost, "/api/v2/elitea_core/pin/prompt_lib/%d/application/5", "auth_core__project_user_role"},
	{http.MethodDelete, "/api/v2/elitea_core/pin/prompt_lib/%d/application/5", "auth_core__project_user_role"},
	{http.MethodPost, "/api/v2/social/pin/prompt_lib/%d/application/5", "auth_core__project_user_role"},
	{http.MethodDelete, "/api/v2/social/pin/prompt_lib/%d/application/5", "auth_core__project_user_role"},
	{http.MethodGet, "/api/v2/social/authors/%d", "auth_core__project_user_role"},
	{http.MethodGet, "/api/v2/social/trending_authors/prompt_lib/%d", "social_likes"},
}

func TestProjectScopedReadsAndPinsAreRefusedToNonMembersAndAdmitMembers(t *testing.T) {
	for _, route := range accessMatrixRoutes {
		path := fmt.Sprintf(route.path, 7)
		for _, actor := range []struct {
			name   string
			member bool
			user   int
		}{
			{"project owner", true, 1},
			{"project member", true, 2},
			{"member of another project", false, 3},
			{"user with no projects", false, 4},
		} {
			for _, kind := range []string{"pat", "session"} {
				t.Run(fmt.Sprintf("%s %s as %s via %s", route.method, path, actor.name, kind), func(t *testing.T) {
					members := accessMatrixMembers()
					router, db := newAccessMatrixRouter(t, members)
					request := httptest.NewRequest(route.method, path, nil)
					credential{kind, actor.user}.apply(request)
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, request)

					if len(members.asked) != 1 || members.asked[0] != [2]int{7, actor.user} {
						t.Fatalf("membership asked about %v, want exactly [{7 %d}]", members.asked, actor.user)
					}
					if actor.member {
						if recorder.Code == http.StatusForbidden || recorder.Code == http.StatusUnauthorized {
							t.Fatalf("status %d for a member; body=%s", recorder.Code, recorder.Body)
						}
						if len(db.sent(route.touches)) == 0 {
							t.Fatalf("a member was admitted but the handler never sent a statement naming %q (status %d); the refusals would prove nothing",
								route.touches, recorder.Code)
						}
						return
					}
					if recorder.Code != http.StatusForbidden {
						t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body)
					}
					if hits := db.sent(protectedFragments...); len(hits) != 0 {
						t.Fatalf("a refused request reached protected tables: %q", hits)
					}
				})
			}
		}

		t.Run(fmt.Sprintf("%s %s unauthenticated", route.method, path), func(t *testing.T) {
			members := accessMatrixMembers()
			router, db := newAccessMatrixRouter(t, members)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(route.method, path, nil))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", recorder.Code)
			}
			if hits := db.sent(protectedFragments...); len(members.asked) != 0 || len(hits) != 0 {
				t.Fatalf("an unauthenticated request reached membership %v / protected tables %q", members.asked, hits)
			}
		})

		t.Run(fmt.Sprintf("%s %s token without an owning user", route.method, path), func(t *testing.T) {
			members := accessMatrixMembers()
			router, db := newAccessMatrixRouter(t, members)
			request := httptest.NewRequest(route.method, path, nil)
			request.Header.Set("Authorization", "Bearer pat-orphan")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", recorder.Code)
			}
			if hits := db.sent(protectedFragments...); len(hits) != 0 {
				t.Fatalf("a token with no owning user reached protected tables: %q", hits)
			}
		})
	}
}

// A malformed project id never reaches a handler or the database.
func TestProjectScopedReadsAndPinsRefuseAMalformedProject(t *testing.T) {
	for _, route := range accessMatrixRoutes {
		for _, project := range []string{"0", "-1", "abc", "7x"} {
			path := strings.Replace(route.path, "%d", project, 1)
			t.Run(route.method+" "+path, func(t *testing.T) {
				router, db := newAccessMatrixRouter(t, accessMatrixMembers())
				request := httptest.NewRequest(route.method, path, nil)
				credential{"pat", 1}.apply(request)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusBadRequest && recorder.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want a 400/403 refusal; body=%s", recorder.Code, recorder.Body)
				}
				if hits := db.sent(append(protectedFragments, route.touches)...); len(hits) != 0 {
					t.Fatalf("a malformed project id reached the database: %q", hits)
				}
			})
		}
	}
}

// The project-less platform settings are what the web app reads on every page
// and stay open to any authenticated caller; the author lookup has no project
// to gate and decides in its handler.
func TestProjectlessRoutesStayReachableToAnyAuthenticatedCaller(t *testing.T) {
	for _, tc := range []struct{ target, touches string }{
		{"/api/v2/elitea_core/platform_settings/prompt_lib", "platform_config"},
		{"/api/v2/elitea_core/author/prompt_lib/2", "auth_core__user"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			members := accessMatrixMembers()
			router, db := newAccessMatrixRouter(t, members)
			request := httptest.NewRequest(http.MethodGet, tc.target, nil)
			credential{"session", 4}.apply(request)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code == http.StatusForbidden || recorder.Code == http.StatusUnauthorized {
				t.Fatalf("status = %d for a signed-in user with no projects; body=%s", recorder.Code, recorder.Body)
			}
			if len(members.asked) != 0 {
				t.Fatalf("a project-less route asked about membership: %v", members.asked)
			}
			if len(db.sent(tc.touches)) == 0 {
				t.Fatalf("the handler never sent a statement naming %q", tc.touches)
			}
		})
	}
	t.Run("author lookup refuses a token with no owning user before any query", func(t *testing.T) {
		router, db := newAccessMatrixRouter(t, accessMatrixMembers())
		request := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/author/prompt_lib/2", nil)
		request.Header.Set("Authorization", "Bearer pat-orphan")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if hits := db.sent("auth_core__"); recorder.Code != http.StatusForbidden || len(hits) != 0 {
			t.Fatalf("status %d, statements %q; want 403 and none", recorder.Code, hits)
		}
	})
	t.Run("author lookup refuses an unauthenticated request", func(t *testing.T) {
		router, db := newAccessMatrixRouter(t, accessMatrixMembers())
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/author/prompt_lib/2", nil))
		if hits := db.sent("auth_core__"); recorder.Code != http.StatusUnauthorized || len(hits) != 0 {
			t.Fatalf("status %d, statements %q; want 401 and none", recorder.Code, hits)
		}
	})
}
