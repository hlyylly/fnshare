// Package fuse exposes the unified fnshare resource library as a read-only
// FUSE filesystem so existing apps (飞牛影视, Plex, Jellyfin, your file
// manager…) can browse and read fnshare files like normal local files.
//
// Layout:
//
//   <mount>/
//     <group-name>/
//       <filename>          shared files in that group
//       .private/
//         <filename>        private files OWNED by us (decrypted name)
//
// Private files we DON'T own are hidden — we can't decrypt them anyway.
//
// M7: reads go through file.Reader → byte-bounded stripe cache → on-demand
// shard fetch. RAM stays bounded regardless of file size, and a single
// FUSE Read only fetches the stripes it overlaps. Media scrubbing is fast
// because adjacent stripes stay hot in cache.
package fuse

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fnshare/fnshare/internal/file"
	"github.com/fnshare/fnshare/internal/group"
	"github.com/fnshare/fnshare/internal/manifest"
	"github.com/fnshare/fnshare/internal/store"

	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fs"
	"go.uber.org/zap"
)

type Service struct {
	mountpoint string
	server     *gofuse.Server
	log        *zap.SugaredLogger
}

type Options struct {
	Mountpoint string
	SelfPeerID string
}

// Mount mounts the filesystem at opts.Mountpoint.
func Mount(opts Options, files *file.Service, st *store.Store, log *zap.SugaredLogger) (*Service, error) {
	if err := os.MkdirAll(opts.Mountpoint, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir mountpoint: %w", err)
	}
	root := &rootNode{
		store: st, files: files, self: opts.SelfPeerID, log: log,
	}
	mountOpts := &fs.Options{
		MountOptions: gofuse.MountOptions{
			Name:          "fnshare",
			FsName:        "fnshare",
			AllowOther:    true,
			DisableXAttrs: true,
		},
		EntryTimeout: ttlPtr(2 * time.Second),
		AttrTimeout:  ttlPtr(2 * time.Second),
	}

	server, err := fs.Mount(opts.Mountpoint, root, mountOpts)
	if err != nil {
		mountOpts.MountOptions.AllowOther = false
		server, err = fs.Mount(opts.Mountpoint, root, mountOpts)
		if err != nil {
			return nil, fmt.Errorf("fuse mount: %w", err)
		}
		log.Warnw("FUSE mounted without allow_other — only the daemon's user can read the mount",
			"hint", "add 'user_allow_other' to /etc/fuse.conf to share with other apps")
	}
	log.Infow("FUSE mounted", "at", opts.Mountpoint)
	return &Service{mountpoint: opts.Mountpoint, server: server, log: log}, nil
}

func (s *Service) Stop() error {
	if s == nil || s.server == nil {
		return nil
	}
	if err := s.server.Unmount(); err != nil {
		return fmt.Errorf("unmount: %w", err)
	}
	s.log.Infow("FUSE unmounted", "at", s.mountpoint)
	return nil
}

// ----- FUSE nodes -----

type rootNode struct {
	fs.Inode

	store *store.Store
	files *file.Service
	self  string
	log   *zap.SugaredLogger
}

var (
	_ fs.NodeReaddirer = (*rootNode)(nil)
	_ fs.NodeLookuper  = (*rootNode)(nil)
	_ fs.NodeGetattrer = (*rootNode)(nil)
)

func (r *rootNode) Getattr(_ context.Context, _ fs.FileHandle, out *gofuse.AttrOut) syscall.Errno {
	out.Mode = 0o555
	return fs.OK
}

func (r *rootNode) Readdir(_ context.Context) (fs.DirStream, syscall.Errno) {
	groups, err := group.ListGroups(r.store)
	if err != nil {
		return nil, syscall.EIO
	}
	entries := make([]gofuse.DirEntry, 0, len(groups))
	for _, g := range groups {
		entries = append(entries, gofuse.DirEntry{
			Name: sanitize(g.Name),
			Mode: syscall.S_IFDIR | 0o555,
		})
	}
	return fs.NewListDirStream(entries), fs.OK
}

func (r *rootNode) Lookup(ctx context.Context, name string, out *gofuse.EntryOut) (*fs.Inode, syscall.Errno) {
	groups, err := group.ListGroups(r.store)
	if err != nil {
		return nil, syscall.EIO
	}
	for _, g := range groups {
		if sanitize(g.Name) != name {
			continue
		}
		child := &groupNode{
			groupID: g.ID, groupName: g.Name,
			files: r.files, self: r.self, log: r.log,
		}
		out.Mode = syscall.S_IFDIR | 0o555
		return r.NewInode(ctx, child, fs.StableAttr{Mode: syscall.S_IFDIR}), fs.OK
	}
	return nil, syscall.ENOENT
}

type groupNode struct {
	fs.Inode

	groupID, groupName string
	files              *file.Service
	self               string
	log                *zap.SugaredLogger
}

var (
	_ fs.NodeReaddirer = (*groupNode)(nil)
	_ fs.NodeLookuper  = (*groupNode)(nil)
	_ fs.NodeGetattrer = (*groupNode)(nil)
)

func (g *groupNode) Getattr(_ context.Context, _ fs.FileHandle, out *gofuse.AttrOut) syscall.Errno {
	out.Mode = 0o555
	return fs.OK
}

func (g *groupNode) listFiles() (shared []*manifest.Manifest, ownedPrivate []*manifest.Manifest) {
	all, _ := g.files.List()
	for _, m := range all {
		if m.GroupID != g.groupID {
			continue
		}
		switch m.Mode {
		case manifest.ModeShared, "":
			shared = append(shared, m)
		case manifest.ModePrivate:
			if m.OwnerPeerID == g.self {
				ownedPrivate = append(ownedPrivate, m)
			}
		}
	}
	return
}

func (g *groupNode) Readdir(_ context.Context) (fs.DirStream, syscall.Errno) {
	shared, ownedPrivate := g.listFiles()
	entries := make([]gofuse.DirEntry, 0, len(shared)+1)
	for _, m := range shared {
		entries = append(entries, gofuse.DirEntry{
			Name: sanitize(visibleName(m, g.self)),
			Mode: syscall.S_IFREG | 0o444,
		})
	}
	if len(ownedPrivate) > 0 {
		entries = append(entries, gofuse.DirEntry{
			Name: ".private",
			Mode: syscall.S_IFDIR | 0o555,
		})
	}
	return fs.NewListDirStream(entries), fs.OK
}

func (g *groupNode) Lookup(ctx context.Context, name string, out *gofuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if name == ".private" {
		_, owned := g.listFiles()
		if len(owned) == 0 {
			return nil, syscall.ENOENT
		}
		child := &privateNode{groupID: g.groupID, files: g.files, self: g.self, log: g.log}
		out.Mode = syscall.S_IFDIR | 0o555
		return g.NewInode(ctx, child, fs.StableAttr{Mode: syscall.S_IFDIR}), fs.OK
	}

	shared, _ := g.listFiles()
	for _, m := range shared {
		if sanitize(visibleName(m, g.self)) != name {
			continue
		}
		child := &fileNode{m: m, files: g.files, log: g.log}
		out.Mode = syscall.S_IFREG | 0o444
		out.Size = uint64(m.Size)
		return g.NewInode(ctx, child, fs.StableAttr{Mode: syscall.S_IFREG}), fs.OK
	}
	return nil, syscall.ENOENT
}

type privateNode struct {
	fs.Inode

	groupID string
	files   *file.Service
	self    string
	log     *zap.SugaredLogger
}

var (
	_ fs.NodeReaddirer = (*privateNode)(nil)
	_ fs.NodeLookuper  = (*privateNode)(nil)
	_ fs.NodeGetattrer = (*privateNode)(nil)
)

func (p *privateNode) Getattr(_ context.Context, _ fs.FileHandle, out *gofuse.AttrOut) syscall.Errno {
	out.Mode = 0o555
	return fs.OK
}

func (p *privateNode) ownedHere() []*manifest.Manifest {
	all, _ := p.files.List()
	var out []*manifest.Manifest
	for _, m := range all {
		if m.GroupID == p.groupID && m.Mode == manifest.ModePrivate && m.OwnerPeerID == p.self {
			out = append(out, m)
		}
	}
	return out
}

func (p *privateNode) Readdir(_ context.Context) (fs.DirStream, syscall.Errno) {
	files := p.ownedHere()
	entries := make([]gofuse.DirEntry, 0, len(files))
	for _, m := range files {
		entries = append(entries, gofuse.DirEntry{
			Name: sanitize(visibleName(m, p.self)),
			Mode: syscall.S_IFREG | 0o444,
		})
	}
	return fs.NewListDirStream(entries), fs.OK
}

func (p *privateNode) Lookup(ctx context.Context, name string, out *gofuse.EntryOut) (*fs.Inode, syscall.Errno) {
	for _, m := range p.ownedHere() {
		if sanitize(visibleName(m, p.self)) != name {
			continue
		}
		child := &fileNode{m: m, files: p.files, log: p.log}
		out.Mode = syscall.S_IFREG | 0o444
		out.Size = uint64(m.Size)
		return p.NewInode(ctx, child, fs.StableAttr{Mode: syscall.S_IFREG}), fs.OK
	}
	return nil, syscall.ENOENT
}

type fileNode struct {
	fs.Inode

	m     *manifest.Manifest
	files *file.Service
	log   *zap.SugaredLogger
}

var (
	_ fs.NodeOpener    = (*fileNode)(nil)
	_ fs.NodeGetattrer = (*fileNode)(nil)
)

func (f *fileNode) Getattr(_ context.Context, _ fs.FileHandle, out *gofuse.AttrOut) syscall.Errno {
	out.Mode = 0o444
	out.Size = uint64(f.m.Size)
	out.Mtime = uint64(f.m.CreatedAt.Unix())
	out.Atime = out.Mtime
	out.Ctime = out.Mtime
	return fs.OK
}

func (f *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_APPEND|syscall.O_TRUNC) != 0 {
		return nil, 0, syscall.EROFS
	}
	rdr, err := f.files.OpenReader(ctx, f.m.FileID)
	if err != nil {
		f.log.Warnw("fuse: cannot open reader", "file", f.m.FileID[:12], "err", err)
		return nil, 0, syscall.EIO
	}
	return &openFile{rdr: rdr}, gofuse.FOPEN_KEEP_CACHE, fs.OK
}

// openFile wraps a file.Reader so the FUSE library can call ReadAt on it.
// Reads are stripe-bounded — a 4 MiB stripe is the largest unit ever
// brought into RAM at once on the read path.
type openFile struct {
	rdr *file.Reader
	mu  sync.Mutex
}

var (
	_ fs.FileReader   = (*openFile)(nil)
	_ fs.FileReleaser = (*openFile)(nil)
)

func (o *openFile) Read(_ context.Context, dst []byte, off int64) (gofuse.ReadResult, syscall.Errno) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.rdr.ReadAt(dst, off)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}
	return gofuse.ReadResultData(dst[:n]), fs.OK
}

func (o *openFile) Release(_ context.Context) syscall.Errno {
	o.mu.Lock()
	defer o.mu.Unlock()
	_ = o.rdr.Close()
	return fs.OK
}

// ----- helpers -----

// visibleName returns the filename to show in a FUSE listing. For private
// files we own, the file.Reader has the decrypted name available; ListFile
// summaries don't, so we use the file_id-based name as a deterministic
// fallback. Real-name display works once the file has been opened once.
func visibleName(m *manifest.Manifest, selfPeerID string) string {
	if m.Mode == manifest.ModePrivate && m.FilenameEncrypted {
		if m.OwnerPeerID != selfPeerID {
			return ""
		}
		return shortID(m) + ".bin"
	}
	if m.Filename != "" {
		return m.Filename
	}
	return shortID(m) + ".bin"
}

func shortID(m *manifest.Manifest) string {
	if len(m.FileID) >= 12 {
		return m.FileID[:12]
	}
	return m.FileID
}

func sanitize(s string) string {
	if s == "" {
		return "_"
	}
	r := strings.NewReplacer("/", "_", "\x00", "_")
	out := r.Replace(s)
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

func ttlPtr(d time.Duration) *time.Duration { return &d }
