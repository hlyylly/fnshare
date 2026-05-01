// Package file orchestrates streaming put/get of files: encrypt + EC each
// stripe independently, distribute to holders chosen by rendezvous hash.
//
// M7: multi-stripe layout — RAM is bounded by StripeDataSize (default 4
// MiB) regardless of file size. A 50 GiB movie streams through ~12.5K
// stripes without ever holding the whole thing in memory.
package file

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/fnshare/fnshare/internal/blockstore"
	"github.com/fnshare/fnshare/internal/crypto"
	"github.com/fnshare/fnshare/internal/ec"
	"github.com/fnshare/fnshare/internal/group"
	"github.com/fnshare/fnshare/internal/holders"
	"github.com/fnshare/fnshare/internal/keys"
	"github.com/fnshare/fnshare/internal/manifest"
	"github.com/fnshare/fnshare/internal/node"
	"github.com/fnshare/fnshare/internal/spool"
	"github.com/fnshare/fnshare/internal/store"

	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"
)

// DefaultStripeDataSize is the plaintext bytes per stripe. With k=2 each
// shard is 2 MiB; with k=4 each shard is 1 MiB. Keep this large enough
// that per-stripe overhead (12B nonce + 16B GCM tag + manifest entry) is
// negligible, but small enough that a single stripe fits comfortably in
// any NAS RAM.
const DefaultStripeDataSize = 4 * 1024 * 1024 // 4 MiB

type Service struct {
	node       *node.Node
	store      *store.Store
	identity   *keys.Identity
	blockstore *blockstore.Store
	spool      *spool.Spool // optional — when set, offline holders get queued instead of failing the upload
	params     ec.Params
	stripeSize int
	cache      *StripeCache
	log        *zap.SugaredLogger
}

func New(n *node.Node, s *store.Store, id *keys.Identity, bs *blockstore.Store,
	params ec.Params, log *zap.SugaredLogger) *Service {
	return &Service{
		node: n, store: s, identity: id, blockstore: bs,
		params: params, stripeSize: DefaultStripeDataSize,
		cache: NewStripeCache(1 * 1024 * 1024 * 1024),
		log:   log,
	}
}

// AttachSpool wires in the spool. Without it, offline holders cause Put
// to fail; with it, shards/manifests for offline peers are queued and
// retried by the spool worker. Daemons should call this; one-shot CLIs
// (like group-join) leave it nil.
func (s *Service) AttachSpool(sp *spool.Spool) { s.spool = sp }

// PutOptions controls how a file is uploaded.
type PutOptions struct {
	GroupID string // required if node is in >1 group
	Mode    string // manifest.ModeShared (default) or manifest.ModePrivate
}

// Put streams body, encrypting + EC-encoding stripe by stripe and shipping
// shards to the deterministic holders chosen by rendezvous hashing.
// Returns the resulting manifest. The caller must Close body.
//
// `_size` is informational (used for nice progress logging only); the
// stream is read to EOF regardless.
func (s *Service) Put(ctx context.Context, filename string, body io.Reader, _size int64, opts PutOptions) (*manifest.Manifest, error) {
	g, err := s.resolveGroup(opts.GroupID)
	if err != nil {
		return nil, err
	}
	mode := opts.Mode
	if mode == "" {
		mode = manifest.ModeShared
	}
	if mode != manifest.ModeShared && mode != manifest.ModePrivate {
		return nil, fmt.Errorf("unknown mode %q", mode)
	}

	// 1. Generate file id and per-file key. file_id is random (NOT a
	//    content hash) so we can pick holders before reading any bytes.
	var fidRaw [16]byte
	if _, err := rand.Read(fidRaw[:]); err != nil {
		return nil, err
	}
	fileID := hex.EncodeToString(fidRaw[:])
	fileKey, err := crypto.NewFileKey()
	if err != nil {
		return nil, err
	}

	// 2. Pick k+m holders for this file via rendezvous. All stripes will
	//    use this same holder set (slot i of every stripe lands on
	//    Holders[i]).
	totalSlots := s.params.DataShards + s.params.ParityShards
	memberIDs, err := s.groupMemberIDs(g.ID)
	if err != nil {
		return nil, err
	}
	if len(memberIDs) < totalSlots {
		return nil, fmt.Errorf("group %s has %d members, need at least %d for %d+%d EC",
			g.ID[:12], len(memberIDs), totalSlots, s.params.DataShards, s.params.ParityShards)
	}
	holderList := holders.Pick(memberIDs, fileID, totalSlots)

	wrapped, displayName, fnEncrypted, err := s.wrapForMode(mode, fileKey, filename, g)
	if err != nil {
		return nil, err
	}

	m := &manifest.Manifest{
		FileID:            fileID,
		GroupID:           g.ID,
		Filename:          displayName,
		DataShards:        s.params.DataShards,
		ParityShards:      s.params.ParityShards,
		StripeDataSize:    s.stripeSize,
		Holders:           holderList,
		CreatedAt:         time.Now().UTC(),
		OwnerPeerID:       s.node.Host.ID().String(),
		Mode:              mode,
		WrappedKey:        wrapped,
		FilenameEncrypted: fnEncrypted,
	}

	// 3. Stream: read up to stripeSize plaintext bytes, encrypt, EC, ship.
	buf := make([]byte, s.stripeSize)
	stripeIdx := 0
	totalBytes := int64(0)
	for {
		n, readErr := io.ReadFull(body, buf)
		if n == 0 && (readErr == io.EOF || readErr == io.ErrUnexpectedEOF) {
			break
		}
		// readErr may be ErrUnexpectedEOF (short final read) or io.EOF;
		// both are fine — we encode whatever we got.
		stripe, err := s.uploadStripe(ctx, fileKey, holderList, fileID, stripeIdx, buf[:n])
		if err != nil {
			return nil, fmt.Errorf("stripe %d: %w", stripeIdx, err)
		}
		m.Stripes = append(m.Stripes, *stripe)
		totalBytes += int64(n)
		stripeIdx++
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read body: %w", readErr)
		}
	}
	if totalBytes == 0 {
		return nil, errors.New("file is empty")
	}
	m.Size = totalBytes

	// 4. Persist + replicate manifest to all holders. Same offline-tolerant
	//    treatment as shards: if a holder is unreachable, queue the
	//    manifest for later delivery so the file is fully self-describing
	//    on every holder once the network heals.
	if err := manifest.Put(s.store, m); err != nil {
		return nil, fmt.Errorf("save manifest: %w", err)
	}
	// For private files: stash the plaintext filename locally (owner-only)
	// so `fnshare ls` and the FUSE mount can show the real name without
	// having to decrypt the file body.
	if mode == manifest.ModePrivate {
		if err := savePrivateName(s.store, m.FileID, filename); err != nil {
			s.log.Warnw("save private name index", "file", m.FileID[:12], "err", err)
		}
	}
	manifestRaw, err := manifest.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	for _, h := range holderList {
		if h == s.node.Host.ID().String() {
			continue
		}
		pid, err := peer.Decode(h)
		if err != nil {
			continue
		}
		if err := s.node.PutManifestRawOnPeer(ctx, pid, m.FileID, manifestRaw); err != nil {
			if s.spool != nil {
				if spoolErr := s.spool.EnqueueManifest(h, m.FileID, manifestRaw); spoolErr == nil {
					s.log.Warnw("manifest replication spooled",
						"file", m.FileID[:12], "holder", h, "err", err)
					continue
				}
			}
			s.log.Warnw("replicate manifest", "holder", h, "err", err)
		}
	}
	s.log.Infow("uploaded",
		"file", fileID[:12], "group", g.ID[:12], "mode", mode,
		"size", totalBytes, "stripes", len(m.Stripes))
	return m, nil
}

// uploadStripe encrypts one chunk and ships its k+m shards to the
// pre-picked holders.
func (s *Service) uploadStripe(ctx context.Context, fileKey []byte,
	holderList []string, fileID string, idx int, plaintext []byte) (*manifest.Stripe, error) {

	ct, err := crypto.SealAES(fileKey, plaintext)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	shards, err := s.params.Encode(ct)
	if err != nil {
		return nil, fmt.Errorf("ec encode: %w", err)
	}

	stripe := &manifest.Stripe{
		Index: idx, PlaintextBytes: len(plaintext), CiphertextBytes: len(ct),
		ShardIDs: make([]string, len(shards)),
	}
	for i, sh := range shards {
		shardID := blockstore.HashOf(sh)
		stripe.ShardIDs[i] = shardID

		holder := holderList[i]
		if holder == s.node.Host.ID().String() {
			if err := s.blockstore.Put(shardID, sh); err != nil {
				return nil, fmt.Errorf("local put shard %d: %w", i, err)
			}
			continue
		}
		pid, err := peer.Decode(holder)
		if err != nil {
			return nil, fmt.Errorf("bad holder %q: %w", holder, err)
		}
		if err := s.node.PutShardOnPeer(ctx, pid, shardID, sh); err != nil {
			// Offline holder? Spool the shard for later delivery rather
			// than failing the upload — as long as ≥ k of (k+m) holders
			// have their shards immediately, the file is readable now,
			// and the spool worker will catch the absent ones up later.
			if s.spool != nil {
				if spoolErr := s.spool.EnqueueShard(holder, shardID, sh); spoolErr == nil {
					s.log.Warnw("holder unreachable — shard spooled",
						"slot", i, "holder", holder, "shard", shardID[:12], "err", err)
					continue
				} else {
					s.log.Warnw("spool enqueue failed", "err", spoolErr)
				}
			}
			return nil, fmt.Errorf("ship shard %d → %s: %w", i, holder, err)
		}
	}
	return stripe, nil
}

// Get streams the entire decrypted file to w. For random-access reads,
// callers should use OpenReader instead.
func (s *Service) Get(ctx context.Context, fileID string, w io.Writer) (*manifest.Manifest, error) {
	r, err := s.OpenReader(ctx, fileID)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if _, err := io.Copy(w, r); err != nil {
		return nil, err
	}
	return r.Manifest(), nil
}

// OpenReader returns a ReaderAt-style handle that fetches+decrypts stripes
// on demand. Used by FUSE for media seeking and by the API for HTTP Range
// requests (M7+ — handler still buffers to RAM today; future work).
func (s *Service) OpenReader(ctx context.Context, fileID string) (*Reader, error) {
	m, err := manifest.Get(s.store, fileID)
	if errors.Is(err, manifest.ErrNotFound) {
		m, err = s.fetchManifest(ctx, fileID)
		if err != nil {
			return nil, err
		}
		_ = manifest.Put(s.store, m)
	} else if err != nil {
		return nil, err
	}

	g, err := group.LoadByID(s.store, m.GroupID)
	if err != nil {
		return nil, fmt.Errorf("we are not part of group %s", m.GroupID)
	}
	fileKey, err := s.unwrapForMode(m, g)
	if err != nil {
		return nil, err
	}
	return &Reader{
		svc: s, m: m, key: fileKey,
		decryptedFilename: maybeDecryptFilename(m, fileKey),
	}, nil
}

func (s *Service) List() ([]*manifest.Manifest, error) {
	all, err := manifest.List(s.store)
	if err != nil {
		return nil, err
	}
	// Enrich: for private files we own, swap in the plaintext filename
	// from our owner-local index so callers (UI, FUSE, CLI ls) get the
	// real name instead of the base64 ciphertext.
	self := s.node.Host.ID().String()
	for _, m := range all {
		if m.Mode == manifest.ModePrivate && m.OwnerPeerID == self && m.FilenameEncrypted {
			if name, ok := loadPrivateName(s.store, m.FileID); ok {
				m.Filename = name
				m.FilenameEncrypted = false // tell consumers it's now plaintext
			}
		}
	}
	return all, nil
}

// ---------- helpers ----------

func (s *Service) groupMemberIDs(gid string) ([]string, error) {
	members, err := group.ListMembers(s.store, gid)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.PeerID)
	}
	sort.Strings(out) // candidate set order is irrelevant to HRW but stable
	return out, nil
}

func (s *Service) resolveGroup(gid string) (*group.Group, error) {
	if gid != "" {
		return group.LoadByID(s.store, gid)
	}
	groups, err := group.ListGroups(s.store)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, errors.New("not a member of any group — create or join one first")
	}
	if len(groups) > 1 {
		return nil, errors.New("you are in multiple groups — please specify --group / ?group=<id>")
	}
	return groups[0], nil
}

func (s *Service) wrapForMode(mode string, fileKey []byte, filename string, g *group.Group) (
	wrapped []byte, displayName string, fnEncrypted bool, err error) {
	switch mode {
	case manifest.ModeShared:
		if len(g.SharedKey) != crypto.FileKeySize {
			return nil, "", false, errors.New("group has no shared key")
		}
		wrapped, err = crypto.SealAES(g.SharedKey, fileKey)
		if err != nil {
			return nil, "", false, err
		}
		return wrapped, filename, false, nil

	case manifest.ModePrivate:
		if len(s.identity.EncPub) != crypto.EncPubSize {
			return nil, "", false, errors.New("local node has no enc keypair")
		}
		wrapped, err = crypto.SealAnonymous(fileKey, s.identity.EncPub)
		if err != nil {
			return nil, "", false, err
		}
		ct, err := crypto.SealAES(fileKey, []byte(filename))
		if err != nil {
			return nil, "", false, err
		}
		return wrapped, base64.RawStdEncoding.EncodeToString(ct), true, nil
	}
	return nil, "", false, fmt.Errorf("unknown mode %q", mode)
}

func (s *Service) unwrapForMode(m *manifest.Manifest, g *group.Group) ([]byte, error) {
	switch m.Mode {
	case manifest.ModeShared, "":
		if m.Mode == "" {
			return nil, errors.New("manifest predates M4 encryption — file unreadable")
		}
		if len(g.SharedKey) != crypto.FileKeySize {
			return nil, errors.New("local group has no shared key — re-join with a fresh invite")
		}
		return crypto.OpenAES(g.SharedKey, m.WrappedKey)

	case manifest.ModePrivate:
		if m.OwnerPeerID != s.node.Host.ID().String() {
			return nil, errors.New("private file: only the owner can decrypt")
		}
		return crypto.OpenAnonymous(m.WrappedKey, s.identity.EncPub, s.identity.EncPriv)
	}
	return nil, fmt.Errorf("unknown manifest mode %q", m.Mode)
}

func maybeDecryptFilename(m *manifest.Manifest, fileKey []byte) string {
	if !m.FilenameEncrypted {
		return m.Filename
	}
	ct, err := base64.RawStdEncoding.DecodeString(m.Filename)
	if err != nil {
		return ""
	}
	pt, err := crypto.OpenAES(fileKey, ct)
	if err != nil {
		return ""
	}
	return string(pt)
}

func (s *Service) fetchManifest(ctx context.Context, fileID string) (*manifest.Manifest, error) {
	members, err := group.AllMembersAcrossGroups(s.store)
	if err != nil {
		return nil, err
	}
	for _, mem := range members {
		if mem.PeerID == s.node.Host.ID().String() {
			continue
		}
		pid, err := peer.Decode(mem.PeerID)
		if err != nil {
			continue
		}
		m, err := s.node.GetManifestFromPeer(ctx, pid, fileID)
		if err != nil {
			s.log.Debugw("fetch manifest", "peer", mem.PeerID, "err", err)
			continue
		}
		return m, nil
	}
	return nil, fmt.Errorf("no peer has manifest for %s", fileID)
}
