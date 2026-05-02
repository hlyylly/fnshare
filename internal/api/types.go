package api

import "time"

// API uses JSON for ergonomics — these payloads are small (status, invites,
// manifests) and JSON survives `curl` and browser dev tools without help.
// Heavier traffic (file uploads, shard transfer) bypasses this layer.

type StatusResponse struct {
	PeerID         string         `json:"peer_id"`
	Nickname       string         `json:"nickname"`
	DataDir        string         `json:"data_dir"`
	ContributedB   int64          `json:"contributed_bytes"`
	ListenAddrs    []string       `json:"listen_addrs"`
	Groups         []GroupSummary `json:"groups"`
	ConnectedPeers []string       `json:"connected_peers"`
}

type GroupSummary struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	IsAdmin bool            `json:"is_admin"`
	Members []MemberSummary `json:"members"`
}

type MemberSummary struct {
	PeerID       string    `json:"peer_id"`
	Nickname     string    `json:"nickname"`
	ContributedB int64     `json:"contributed_bytes"`
	JoinedAt     time.Time `json:"joined_at"`
	IsOnline     bool      `json:"is_online"`
	Reputation   int64     `json:"reputation"`
	OfflineSince time.Time `json:"offline_since,omitempty"`
}

type InviteRequest struct {
	GroupID   string   `json:"group_id"`
	Bootstrap []string `json:"bootstrap"`
	TTLHours  int      `json:"ttl_hours"`
	QuotaGB   int64    `json:"quota_gb"`
}

type InviteResponse struct {
	Link string `json:"link"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// FileSummary is one row in the GET /v1/files response.
type FileSummary struct {
	FileID    string `json:"file_id"`
	GroupID   string `json:"group_id"`
	GroupName string `json:"group_name"`
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"created_at"`
	Owner     string `json:"owner"`
	Mode      string `json:"mode"` // "shared" or "private"
	Encrypted bool   `json:"filename_encrypted"`
}

type FilesResponse struct {
	Files []FileSummary `json:"files"`
}

// GroupCreateRequest creates a new group on the daemon (move from CLI to UI).
type GroupCreateRequest struct {
	Name string `json:"name"`
}

// GroupJoinRequest joins via invite link — alternative to the offline CLI flow.
type GroupJoinRequest struct {
	InviteLink string `json:"invite_link"`
}

// AuthState reports the daemon's authentication posture so the UI knows
// whether to show the "set password" panel, the "log in" panel, or the
// main app.
type AuthState struct {
	State string `json:"state"` // "no_password" | "needs_login" | "ok"
}

type AuthRequest struct {
	Password string `json:"password"`
}

type AuthResponse struct {
	OK    bool   `json:"ok"`
	Token string `json:"token,omitempty"`
}

// ConfigPublic is the subset of daemon config exposed via /v1/config —
// PublicHost is what the user can edit from the UI to make their invite
// links survive IP changes (the daemon constructs /dns4/<host>/...
// bootstraps automatically).
type ConfigPublic struct {
	Nickname     string `json:"nickname"`
	PublicHost   string `json:"public_host"`
	PublicPort   int    `json:"public_port"`
	ContributedB int64  `json:"contributed_bytes"`
}
