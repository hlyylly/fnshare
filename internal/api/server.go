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
	"time"

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
	Log      *zap.SugaredLogger
}

type Server struct {
	deps  Deps
	httpd *http.Server
}

func New(d Deps) *Server {
	mux := http.NewServeMux()
	s := &Server{deps: d}

	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/groups", s.handleListGroups)
	mux.HandleFunc("POST /v1/groups", s.handleCreateGroup)
	mux.HandleFunc("POST /v1/groups/join", s.handleJoinGroup)
	mux.HandleFunc("POST /v1/invite", s.handleInvite)
	mux.HandleFunc("GET /v1/files", s.handleListFiles)
	mux.HandleFunc("POST /v1/files", s.handleUpload)
	mux.HandleFunc("GET /v1/files/{id}/content", s.handleDownload)
	mux.HandleFunc("GET /v1/ledger", s.handleLedger)

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
	if len(req.Bootstrap) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("bootstrap is required"))
		return
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
