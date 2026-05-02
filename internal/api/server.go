package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/fnshare/fnshare/internal/auth"
	"github.com/fnshare/fnshare/internal/config"
	"github.com/fnshare/fnshare/internal/file"
	"github.com/fnshare/fnshare/internal/group"
	"github.com/fnshare/fnshare/internal/invite"
	"github.com/fnshare/fnshare/internal/keys"
	"github.com/fnshare/fnshare/internal/ledger"
	"github.com/fnshare/fnshare/internal/node"
	"github.com/fnshare/fnshare/internal/store"

	"github.com/libp2p/go-libp2p/core/host"
	"go.uber.org/zap"
)

type Deps struct {
	Cfg      config.Config
	Store    *store.Store
	Identity *keys.Identity
	Host     host.Host
	Node     *node.Node // for join (needs ConnectToPeers + JoinViaPeer)
	Files    *file.Service
	Ledger   *ledger.Ledger
	Auth     *auth.Service // password / session / CLI token
	Log      *zap.SugaredLogger
}

type Server struct {
	deps  Deps
	httpd *http.Server
	mu    sync.RWMutex // guards live cfg mutations from PATCH /v1/config
}

const sessionCookie = "fnshare_session"

func New(d Deps) *Server {
	mux := http.NewServeMux()
	s := &Server{deps: d}

	// Auth endpoints (NO middleware — these gate access to everything else).
	mux.HandleFunc("GET /v1/auth/state", s.handleAuthState)
	mux.HandleFunc("POST /v1/auth/init", s.handleAuthInit)
	mux.HandleFunc("POST /v1/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /v1/auth/logout", s.handleAuthLogout)

	// Everything else requires a valid session cookie OR the CLI token.
	gate := s.requireAuth
	mux.Handle("GET /v1/status", gate(http.HandlerFunc(s.handleStatus)))
	mux.Handle("GET /v1/groups", gate(http.HandlerFunc(s.handleListGroups)))
	mux.Handle("POST /v1/groups", gate(http.HandlerFunc(s.handleCreateGroup)))
	mux.Handle("POST /v1/groups/join", gate(http.HandlerFunc(s.handleJoinGroup)))
	mux.Handle("POST /v1/invite", gate(http.HandlerFunc(s.handleInvite)))
	mux.Handle("GET /v1/files", gate(http.HandlerFunc(s.handleListFiles)))
	mux.Handle("POST /v1/files", gate(http.HandlerFunc(s.handleUpload)))
	mux.Handle("GET /v1/files/{id}/content", gate(http.HandlerFunc(s.handleDownload)))
	mux.Handle("GET /v1/ledger", gate(http.HandlerFunc(s.handleLedger)))
	mux.Handle("GET /v1/config", gate(http.HandlerFunc(s.handleGetConfig)))
	mux.Handle("PATCH /v1/config", gate(http.HandlerFunc(s.handlePatchConfig)))

	// Static UI: served unauthenticated so the browser can load the
	// login screen markup. The JS then calls /v1/auth/state to decide
	// what to render.
	mux.Handle("/", http.FileServer(http.FS(staticFS())))

	s.httpd = &http.Server{
		Addr:              d.Cfg.APIListen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.deps.Cfg.APIListen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.deps.Cfg.APIListen, err)
	}
	go func() {
		if err := s.httpd.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.deps.Log.Errorw("api server", "err", err)
		}
	}()
	s.deps.Log.Infow("api listening", "addr", s.deps.Cfg.APIListen)
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	return s.httpd.Shutdown(ctx)
}

// ---------- auth middleware + handlers ----------

// extractToken pulls the auth token from either the session cookie OR the
// X-Auth-Token header (used by the CLI client which has no cookie jar).
func extractToken(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		return c.Value
	}
	return r.Header.Get("X-Auth-Token")
}

// requireAuth wraps a handler so it 401s without a valid session.
//
// Order matters:
//
//  1. CLI token is checked FIRST — `fnshare status / put / get` always work
//     for the local container owner regardless of UI password state.
//  2. If no password set yet (fresh install), browser users get a 401 with
//     a hint to call /v1/auth/init.
//  3. Otherwise a valid session cookie is required.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Auth.ValidateSession(extractToken(r)) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.deps.Auth.HasPassword() {
			writeErr(w, http.StatusUnauthorized,
				errors.New("daemon has no password set — open the Web UI to set one, or use the CLI inside the container"))
			return
		}
		writeErr(w, http.StatusUnauthorized, errors.New("login required"))
	})
}

func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	state := "ok"
	if !s.deps.Auth.HasPassword() {
		state = "no_password"
	} else if !s.deps.Auth.ValidateSession(extractToken(r)) {
		state = "needs_login"
	}
	writeJSON(w, http.StatusOK, AuthState{State: state})
}

func (s *Server) handleAuthInit(w http.ResponseWriter, r *http.Request) {
	if s.deps.Auth.HasPassword() {
		writeErr(w, http.StatusConflict, errors.New("password already set — use UI to change it"))
		return
	}
	var req AuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.deps.Auth.SetPassword(req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Auto-login the user with the just-set password.
	tok, err := s.deps.Auth.VerifyAndIssue(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	setSessionCookie(w, tok)
	writeJSON(w, http.StatusOK, AuthResponse{OK: true, Token: tok})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req AuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	tok, err := s.deps.Auth.VerifyAndIssue(req.Password)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	setSessionCookie(w, tok)
	writeJSON(w, http.StatusOK, AuthResponse{OK: true, Token: tok})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if tok := extractToken(r); tok != "" {
		s.deps.Auth.Logout(tok)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, AuthResponse{OK: true})
}

func setSessionCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		MaxAge: 30 * 24 * 3600, // 30 days
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

// ---------- config (DDNS host) ----------

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	port := s.deps.Cfg.PublicPort
	if port == 0 {
		port = 4001
	}
	writeJSON(w, http.StatusOK, ConfigPublic{
		Nickname:     s.deps.Cfg.Nickname,
		PublicHost:   s.deps.Cfg.PublicHost,
		PublicPort:   port,
		ContributedB: s.deps.Cfg.ContributedBytes,
	})
}

func (s *Server) handlePatchConfig(w http.ResponseWriter, r *http.Request) {
	var patch struct {
		PublicHost *string `json:"public_host,omitempty"`
		PublicPort *int    `json:"public_port,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	if patch.PublicHost != nil {
		s.deps.Cfg.PublicHost = *patch.PublicHost
	}
	if patch.PublicPort != nil {
		s.deps.Cfg.PublicPort = *patch.PublicPort
	}
	cfg := s.deps.Cfg
	s.mu.Unlock()
	if err := config.Save(cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.handleGetConfig(w, r)
}

// ---------- handlers ----------

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	resp := StatusResponse{
		PeerID:       s.deps.Identity.PeerID.String(),
		Nickname:     s.deps.Cfg.Nickname,
		DataDir:      s.deps.Cfg.DataDir,
		ContributedB: s.deps.Cfg.ContributedBytes,
	}
	for _, a := range s.deps.Host.Addrs() {
		resp.ListenAddrs = append(resp.ListenAddrs, a.String())
	}
	for _, p := range s.deps.Host.Network().Peers() {
		resp.ConnectedPeers = append(resp.ConnectedPeers, p.String())
	}
	groups, _ := group.ListGroups(s.deps.Store)
	for _, g := range groups {
		resp.Groups = append(resp.Groups, GroupSummary{
			ID: g.ID, Name: g.Name, IsAdmin: g.IsAdminNode,
			Members: s.membersWithLiveness(g.ID),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListGroups(w http.ResponseWriter, _ *http.Request) {
	groups, err := group.ListGroups(s.deps.Store)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]GroupSummary, 0, len(groups))
	for _, g := range groups {
		out = append(out, GroupSummary{
			ID: g.ID, Name: g.Name, IsAdmin: g.IsAdminNode,
			Members: s.membersWithLiveness(g.ID),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

// membersWithLiveness joins each group member with the heartbeat-derived
// online/reputation data the ledger carries. Self is always reported as
// online with max reputation (we obviously aren't pinging ourselves).
func (s *Server) membersWithLiveness(gid string) []MemberSummary {
	members, _ := group.ListMembers(s.deps.Store, gid)
	if len(members) == 0 {
		return nil
	}
	// Snapshot the ledger once for cheap lookups.
	repByPeer := map[string]struct {
		online bool
		rep    int64
		off    time.Time
	}{}
	if s.deps.Ledger != nil {
		entries, _ := s.deps.Ledger.All()
		for _, e := range entries {
			repByPeer[e.PeerID] = struct {
				online bool
				rep    int64
				off    time.Time
			}{e.IsOnline, e.Reputation, e.OfflineSince}
		}
	}
	self := s.deps.Identity.PeerID.String()
	out := make([]MemberSummary, 0, len(members))
	for _, m := range members {
		ms := MemberSummary{
			PeerID:       m.PeerID,
			Nickname:     m.Nickname,
			ContributedB: m.ContributedB,
			JoinedAt:     m.JoinedAt,
		}
		if m.PeerID == self {
			ms.IsOnline = true
			ms.Reputation = 100
		} else if r, ok := repByPeer[m.PeerID]; ok {
			ms.IsOnline = r.online
			ms.Reputation = r.rep
			ms.OfflineSince = r.off
		}
		out = append(out, ms)
	}
	return out
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var req GroupCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	g, err := group.Create(req.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := group.Save(s.deps.Store, g); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Add self as the first member (admin).
	pubRaw, err := s.deps.Identity.PubKey.Raw()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().UTC()
	sig, err := g.AdmitMember(s.deps.Identity.PeerID.String(), now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := group.PutMember(s.deps.Store, g.ID, &group.Member{
		PeerID:        s.deps.Identity.PeerID.String(),
		Nickname:      s.deps.Cfg.Nickname,
		NodePub:       pubRaw,
		EncPub:        s.deps.Identity.EncPub,
		ContributedB:  s.deps.Cfg.ContributedBytes,
		JoinedAt:      now,
		AdmittedBySig: sig,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, GroupSummary{
		ID: g.ID, Name: g.Name, IsAdmin: true,
	})
}

func (s *Server) handleJoinGroup(w http.ResponseWriter, r *http.Request) {
	var req GroupJoinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	inv, err := invite.Decode(req.InviteLink)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode invite: %w", err))
		return
	}
	if existing, err := group.LoadByID(s.deps.Store, inv.GroupID); err == nil {
		writeErr(w, http.StatusConflict, fmt.Errorf("already in group %q", existing.Name))
		return
	}
	pid, err := s.deps.Node.ConnectToPeers(r.Context(), inv.BootstrapPeers)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("connect to bootstrap: %w", err))
		return
	}
	resp, err := s.deps.Node.JoinViaPeer(r.Context(), pid, inv)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := group.SaveBootstrap(s.deps.Store, inv.GroupID, inv.BootstrapPeers); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group_id":   resp.GroupID,
		"group_name": resp.GroupName,
		"members":    len(resp.Members),
	})
}

func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	var req InviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Auto-derive bootstrap from PublicHost if user didn't provide one.
	// Lets invite links survive IP changes — DDNS resolves at use time.
	if len(req.Bootstrap) == 0 {
		s.mu.RLock()
		host := s.deps.Cfg.PublicHost
		port := s.deps.Cfg.PublicPort
		s.mu.RUnlock()
		if host == "" {
			writeErr(w, http.StatusBadRequest, errors.New(
				"bootstrap is required — set public_host in config (UI: 概览 → 我的对外地址) or pass bootstrap explicitly"))
			return
		}
		if port == 0 {
			port = 4001
		}
		req.Bootstrap = []string{
			fmt.Sprintf("/dns4/%s/tcp/%d/p2p/%s", host, port, s.deps.Identity.PeerID.String()),
		}
	}

	gid := req.GroupID
	if gid == "" {
		groups, _ := group.ListGroups(s.deps.Store)
		if len(groups) != 1 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("specify group_id; you are in %d groups", len(groups)))
			return
		}
		gid = groups[0].ID
	}
	g, err := group.LoadByID(s.deps.Store, gid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if !g.IsAdminNode {
		writeErr(w, http.StatusForbidden, fmt.Errorf("not the admin of group %s", gid[:12]))
		return
	}
	ttl := time.Duration(req.TTLHours) * time.Hour
	if ttl == 0 {
		ttl = 72 * time.Hour
	}
	var quota int64
	if req.QuotaGB > 0 {
		quota = req.QuotaGB * 1024 * 1024 * 1024
	}
	inv, err := invite.Create(g, req.Bootstrap, ttl, quota)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	link, err := inv.Encode()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, InviteResponse{Link: link})
}

func (s *Server) handleListFiles(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Files == nil {
		writeJSON(w, http.StatusOK, FilesResponse{})
		return
	}
	ms, err := s.deps.Files.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Cache group names so we don't re-load from BadgerDB per file.
	groupNames := map[string]string{}
	if gs, err := group.ListGroups(s.deps.Store); err == nil {
		for _, g := range gs {
			groupNames[g.ID] = g.Name
		}
	}
	resp := FilesResponse{}
	for _, m := range ms {
		resp.Files = append(resp.Files, FileSummary{
			FileID:    m.FileID,
			GroupID:   m.GroupID,
			GroupName: groupNames[m.GroupID],
			Filename:  m.Filename,
			Size:      m.Size,
			CreatedAt: m.CreatedAt.Unix(),
			Owner:     m.OwnerPeerID,
			Mode:      m.Mode,
			Encrypted: m.FilenameEncrypted,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.deps.Files == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("file service not initialized"))
		return
	}
	filename := r.URL.Query().Get("name")
	if filename == "" {
		filename = "unnamed"
	}
	mode := r.URL.Query().Get("mode")
	groupID := r.URL.Query().Get("group")

	sizeStr := r.Header.Get("Content-Length")
	size, _ := strconv.ParseInt(sizeStr, 10, 64)

	m, err := s.deps.Files.Put(r.Context(), filename, r.Body, size,
		file.PutOptions{Mode: mode, GroupID: groupID})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if s.deps.Files == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("file service not initialized"))
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, errors.New("missing file id"))
		return
	}
	// Open the file as a Reader so we can answer with the correct
	// Content-Length up-front and stream stripes on demand. Errors that
	// happen BEFORE the first byte (manifest fetch, key unwrap) become
	// proper 500s; errors mid-stream just close the connection.
	rdr, err := s.deps.Files.OpenReader(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rdr.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(rdr.Size(), 10))
	if _, err := io.Copy(w, rdr); err != nil {
		s.deps.Log.Warnw("download mid-stream", "id", id[:12], "err", err)
	}
}

func (s *Server) handleLedger(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Ledger == nil {
		writeJSON(w, http.StatusOK, map[string]any{"entries": []any{}})
		return
	}
	entries, err := s.deps.Ledger.All()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, ErrorResponse{Error: err.Error()})
}
