package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fnshare/fnshare/internal/api"
	"github.com/fnshare/fnshare/internal/auth"
	"github.com/fnshare/fnshare/internal/blockstore"
	"github.com/fnshare/fnshare/internal/config"
	"github.com/fnshare/fnshare/internal/ec"
	"github.com/fnshare/fnshare/internal/file"
	fnfuse "github.com/fnshare/fnshare/internal/fuse"
	"github.com/fnshare/fnshare/internal/group"
	"github.com/fnshare/fnshare/internal/heartbeat"
	"github.com/fnshare/fnshare/internal/invite"
	"github.com/fnshare/fnshare/internal/keys"
	"github.com/fnshare/fnshare/internal/ledger"
	"github.com/fnshare/fnshare/internal/manifest"
	"github.com/fnshare/fnshare/internal/node"
	"github.com/fnshare/fnshare/internal/repair"
	"github.com/fnshare/fnshare/internal/spool"
	"github.com/fnshare/fnshare/internal/store"
	"strconv"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var (
	flagDataDir string
	logger      *zap.SugaredLogger
)

func main() {
	root := &cobra.Command{
		Use:   "fnshare",
		Short: "fnshare — friend-circle distributed storage on fnOS",
	}
	root.PersistentFlags().StringVar(&flagDataDir, "data", defaultDataDir(), "data directory")

	root.AddCommand(
		cmdInit(),
		cmdGroupCreate(),
		cmdInviteCreate(),
		cmdGroupJoin(),
		cmdGroups(),
		cmdDaemon(),
		cmdStatus(),
		cmdPut(),
		cmdGet(),
		cmdLs(),
	)

	cobra.OnInitialize(func() {
		zl, _ := zap.NewProduction()
		logger = zl.Sugar()
	})

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func defaultDataDir() string {
	if v := os.Getenv("FNSHARE_DATA"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".fnshare"
	}
	return filepath.Join(home, ".fnshare")
}

// ---------------- offline commands (no daemon needed) ----------------

func cmdInit() *cobra.Command {
	var nickname string
	var contributedGB int64
	c := &cobra.Command{
		Use:   "init",
		Short: "Initialize this node (generate identity + default config)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := os.MkdirAll(flagDataDir, 0o700); err != nil {
				return err
			}
			cfg := config.Default(flagDataDir)
			if nickname != "" {
				cfg.Nickname = nickname
			}
			if contributedGB > 0 {
				cfg.ContributedBytes = contributedGB * 1024 * 1024 * 1024
			}
			if err := config.Save(cfg); err != nil {
				return err
			}
			id, err := keys.LoadOrCreate(filepath.Join(flagDataDir, "node.key"))
			if err != nil {
				return err
			}
			fmt.Printf("✓ initialized %s\n  peer id : %s\n  nickname: %s\n  quota   : %.1f GiB\n",
				flagDataDir, id.PeerID, cfg.Nickname,
				float64(cfg.ContributedBytes)/(1024*1024*1024))
			return nil
		},
	}
	c.Flags().StringVar(&nickname, "nickname", "", "display name for this node")
	c.Flags().Int64Var(&contributedGB, "contribute-gb", 0, "GiB to contribute to the group")
	return c
}

func cmdGroupCreate() *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "group-create",
		Short: "Create a new group; this node becomes its admin (you can be in multiple groups)",
		RunE: func(_ *cobra.Command, _ []string) error {
			s, cfg, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			g, err := group.Create(name)
			if err != nil {
				return err
			}
			if err := group.Save(s, g); err != nil {
				return err
			}

			id, err := keys.LoadOrCreate(filepath.Join(cfg.DataDir, "node.key"))
			if err != nil {
				return err
			}
			pubRaw, err := id.PubKey.Raw()
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			sig, err := g.AdmitMember(id.PeerID.String(), now)
			if err != nil {
				return err
			}
			adminMember := &group.Member{
				PeerID:        id.PeerID.String(),
				Nickname:      cfg.Nickname,
				NodePub:       pubRaw,
				EncPub:        id.EncPub,
				ContributedB:  cfg.ContributedBytes,
				JoinedAt:      now,
				AdmittedBySig: sig,
			}
			if err := group.PutMember(s, g.ID, adminMember); err != nil {
				return err
			}

			fmt.Printf("✓ created group %q\n  id: %s\n  share invites with `fnshare invite-create --group %s`\n",
				g.Name, g.ID, g.ID[:12])
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "human-readable group name (required)")
	_ = c.MarkFlagRequired("name")
	return c
}

func cmdInviteCreate() *cobra.Command {
	var groupID string
	var bootstrap []string
	var ttlHours int
	var quotaGB int64
	c := &cobra.Command{
		Use:   "invite-create",
		Short: "Generate an invite link for a group (admin only)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if len(bootstrap) == 0 {
				return fmt.Errorf("--bootstrap is required")
			}
			cfg, err := config.Load(flagDataDir)
			if err != nil {
				return err
			}
			cli := api.NewClient(cfg.APIListen)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if cli.Reachable(ctx) {
				resp, err := cli.Invite(ctx, api.InviteRequest{
					GroupID: groupID, Bootstrap: bootstrap, TTLHours: ttlHours, QuotaGB: quotaGB,
				})
				if err != nil {
					return err
				}
				fmt.Println(resp.Link)
				return nil
			}
			return inviteFromDB(cfg, groupID, bootstrap, ttlHours, quotaGB)
		},
	}
	c.Flags().StringVar(&groupID, "group", "", "group id (required if you're in more than one)")
	c.Flags().StringSliceVar(&bootstrap, "bootstrap", nil, "bootstrap multiaddrs (repeatable)")
	c.Flags().IntVar(&ttlHours, "ttl-hours", 72, "invite validity period in hours")
	c.Flags().Int64Var(&quotaGB, "quota-gb", 0, "max GiB the joining node may consume (0 = unlimited)")
	return c
}

func cmdGroupJoin() *cobra.Command {
	c := &cobra.Command{
		Use:   "group-join <invite-link>",
		Short: "Join a group via invite link (you can be in multiple groups simultaneously)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()

			inv, err := invite.Decode(args[0])
			if err != nil {
				return fmt.Errorf("decode invite: %w", err)
			}
			s, cfg, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			if existing, err := group.LoadByID(s, inv.GroupID); err == nil {
				return fmt.Errorf("already in group %q (%s)", existing.Name, existing.ID[:12])
			}

			id, err := keys.LoadOrCreate(filepath.Join(cfg.DataDir, "node.key"))
			if err != nil {
				return err
			}
			n, err := node.New(ctx, node.Options{
				Cfg: cfg, Identity: id, Store: s, Log: logger,
			})
			if err != nil {
				return err
			}
			defer n.Close()

			pid, err := n.ConnectToPeers(ctx, inv.BootstrapPeers)
			if err != nil {
				return fmt.Errorf("connect to bootstrap peers: %w", err)
			}
			fmt.Printf("• connected to bootstrap peer %s\n", pid)
			resp, err := n.JoinViaPeer(ctx, pid, inv)
			if err != nil {
				return err
			}
			if err := group.SaveBootstrap(s, inv.GroupID, inv.BootstrapPeers); err != nil {
				return fmt.Errorf("save bootstrap: %w", err)
			}
			fmt.Printf("✓ joined group %q (%s) — %d existing member(s)\n",
				resp.GroupName, resp.GroupID[:12], len(resp.Members))
			return nil
		},
	}
	return c
}

func cmdGroups() *cobra.Command {
	c := &cobra.Command{
		Use:   "groups",
		Short: "List all groups this node belongs to (uses daemon API if running)",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(flagDataDir)
			if err != nil {
				return err
			}
			cli := api.NewClient(cfg.APIListen)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			var groups []apiGroupRow
			if cli.Reachable(ctx) {
				resp, err := cli.Status(ctx)
				if err != nil {
					return err
				}
				for _, g := range resp.Groups {
					groups = append(groups, apiGroupRow{
						ID: g.ID, Name: g.Name, IsAdmin: g.IsAdmin, MemberCount: len(g.Members),
					})
				}
			} else {
				s, err := store.Open(cfg.DataDir)
				if err != nil {
					return err
				}
				defer s.Close()
				gs, err := group.ListGroups(s)
				if err != nil {
					return err
				}
				for _, g := range gs {
					ms, _ := group.ListMembers(s, g.ID)
					groups = append(groups, apiGroupRow{
						ID: g.ID, Name: g.Name, IsAdmin: g.IsAdminNode, MemberCount: len(ms),
					})
				}
			}
			if len(groups) == 0 {
				fmt.Println("(no groups — create one with `group-create` or join one with `group-join`)")
				return nil
			}
			for _, g := range groups {
				role := "member"
				if g.IsAdmin {
					role = "admin"
				}
				// Print FULL group id so it can be passed to other commands
				// without lookup; name follows for human reading.
				fmt.Printf("%s  %-20s  [%s]  %d member(s)\n",
					g.ID, g.Name, role, g.MemberCount)
			}
			return nil
		},
	}
	return c
}

type apiGroupRow struct {
	ID, Name    string
	IsAdmin     bool
	MemberCount int
}

// ---------------- daemon ----------------

func cmdDaemon() *cobra.Command {
	c := &cobra.Command{
		Use:   "daemon",
		Short: "Run the fnshare node (libp2p + block store + local API)",
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			s, cfg, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			id, err := keys.LoadOrCreate(filepath.Join(cfg.DataDir, "node.key"))
			if err != nil {
				return err
			}
			n, err := node.New(ctx, node.Options{
				Cfg: cfg, Identity: id, Store: s, Log: logger,
			})
			if err != nil {
				return err
			}
			defer n.Close()

			bs, err := blockstore.Open(cfg.DataDir)
			if err != nil {
				return err
			}
			n.AttachBlockstore(bs)

			led := ledger.New(s)
			n.AttachLedger(led)

			go func() {
				tick := time.NewTicker(10 * time.Second)
				defer tick.Stop()
				for {
					select {
					case <-ctx.Done():
						_ = led.Flush()
						return
					case <-tick.C:
						if err := led.Flush(); err != nil {
							logger.Warnw("ledger flush", "err", err)
						}
					}
				}
			}()

			fileSvc := file.New(n, s, id, bs, ec.Default(), logger)

			// Spool: queue shards/manifests for offline holders so that one
			// friend's NAS being down doesn't block uploads. Worker drains
			// the queue when peers come back online (heartbeat-driven).
			sp, err := spool.Open(cfg.DataDir)
			if err != nil {
				return err
			}
			fileSvc.AttachSpool(sp)
			spoolWorker := spool.NewWorker(sp, n, led, 10*time.Second, logger)
			go spoolWorker.Run(ctx)

			repairSvc := repair.New(n, s, bs, led, logger)

			// Dial bootstrap peers for ALL groups, then re-poll on a timer
			// so newly joined members get discovered.
			go func() {
				time.Sleep(1 * time.Second)
				n.BootstrapAllGroups(ctx)
				n.SyncGroupMembers(ctx)
				tick := time.NewTicker(5 * time.Second)
				defer tick.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-tick.C:
						n.BootstrapAllGroups(ctx)
						// Member-list gossip: lets admin learn about
						// members who joined via non-admin peers (e.g.
						// when admin's IPv6 was unreachable from the
						// joining node's network at join time).
						n.SyncGroupMembers(ctx)
					}
				}
			}()

			// Heartbeat: ping every member periodically; transitions trigger
			// a repair scan. Interval comes from config / env (test setups
			// override to a few seconds for fast offline detection).
			interval := time.Duration(cfg.HeartbeatSeconds) * time.Second
			if v := os.Getenv("FNSHARE_HEARTBEAT_SECONDS"); v != "" {
				if n2, err := strconv.Atoi(v); err == nil && n2 > 0 {
					interval = time.Duration(n2) * time.Second
				}
			}
			if interval <= 0 {
				interval = 30 * time.Second
			}
			hb := heartbeat.New(n, s, led, interval,
				func(peerID string, wentOffline bool) {
					if !wentOffline {
						return
					}
					go repairSvc.ScanForOfflinePeer(ctx, peerID)
				}, logger)
			go hb.Run(ctx)

			// FUSE mount: optional. Only when cfg.MountPath is set; lets
			// other apps on this host read fnshare files as a normal dir.
			mountPath := cfg.MountPath
			if v := os.Getenv("FNSHARE_MOUNT"); v != "" {
				mountPath = v
			}
			var mountSvc *fnfuse.Service
			if mountPath != "" {
				mountSvc, err = fnfuse.Mount(fnfuse.Options{
					Mountpoint: mountPath,
					SelfPeerID: id.PeerID.String(),
				}, fileSvc, s, logger)
				if err != nil {
					logger.Warnw("FUSE mount failed (continuing without)", "path", mountPath, "err", err)
				} else {
					defer func() { _ = mountSvc.Stop() }()
				}
			}

			authSvc, err := auth.New(s, cfg.DataDir)
			if err != nil {
				return err
			}

			apiSrv := api.New(api.Deps{
				Cfg: cfg, Store: s, Identity: id, Host: n.Host,
				Node: n, Files: fileSvc, Ledger: led, Auth: authSvc, Log: logger,
			})
			if err := apiSrv.Start(); err != nil {
				return err
			}
			defer func() {
				stopCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
				defer c()
				_ = apiSrv.Stop(stopCtx)
			}()

			fmt.Printf("fnshare daemon\n")
			fmt.Printf("  peer id: %s\n", n.Host.ID())
			fmt.Printf("  api+ui : http://%s\n", cfg.APIListen)
			if mountSvc != nil {
				fmt.Printf("  mount  : %s  (read-only FUSE)\n", mountPath)
			}
			fmt.Printf("  listen :\n")
			for _, a := range n.SelfMultiaddrs() {
				fmt.Printf("    %s\n", a)
			}
			groups := n.Groups()
			if len(groups) == 0 {
				fmt.Printf("  groups : <none>\n")
			} else {
				fmt.Printf("  groups : %d\n", len(groups))
				for _, g := range groups {
					role := "member"
					if g.IsAdminNode {
						role = "admin"
					}
					fmt.Printf("    %s  %s  [%s]\n", g.ID[:12], g.Name, role)
				}
			}

			sigc := make(chan os.Signal, 1)
			signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
			<-sigc
			fmt.Println("\nshutting down…")
			return nil
		},
	}
	return c
}

// ---------------- API-backed commands ----------------

func cmdStatus() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Show node + groups status (uses daemon API if running, falls back to local DB)",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(flagDataDir)
			if err != nil {
				return err
			}
			cli := api.NewClient(cfg.APIListen)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			if cli.Reachable(ctx) {
				resp, err := cli.Status(ctx)
				if err != nil {
					return err
				}
				printStatus(resp)
				return nil
			}
			return statusFromDB(cfg)
		},
	}
	return c
}

func cmdPut() *cobra.Command {
	var private bool
	var groupID string
	c := &cobra.Command{
		Use:   "put <file>",
		Short: "Upload a file (encrypted, EC-encoded across one group's members)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := config.Load(flagDataDir)
			if err != nil {
				return err
			}
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()
			st, err := f.Stat()
			if err != nil {
				return err
			}

			cli := api.NewClient(cfg.APIListen)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if !cli.Reachable(ctx) {
				return fmt.Errorf("daemon not running on %s", cfg.APIListen)
			}
			mode := "shared"
			if private {
				mode = "private"
			}
			raw, err := cli.Upload(ctx, filepath.Base(args[0]), f, st.Size(), mode, groupID)
			if err != nil {
				return err
			}
			var m manifest.Manifest
			if err := json.Unmarshal(raw, &m); err != nil {
				fmt.Println(string(raw))
				return nil
			}
			fmt.Printf("✓ uploaded\n  file id : %s\n  group   : %s\n  mode    : %s\n  size    : %d bytes\n  layout  : %d+%d EC, %d stripes × %d B\n",
				m.FileID, m.GroupID[:12], m.Mode, m.Size,
				m.DataShards, m.ParityShards, len(m.Stripes), m.StripeDataSize)
			for i, h := range m.Holders {
				fmt.Printf("  slot %d → %s\n", i, shortPeer([]string{h}))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&private, "private", false, "encrypt to YOURSELF only — other members hold shards but cannot decrypt")
	c.Flags().StringVar(&groupID, "group", "", "group id (required if you're in more than one)")
	return c
}

func cmdGet() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <file-id> <output-path>",
		Short: "Download a file by id, reconstructing from available shards",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := config.Load(flagDataDir)
			if err != nil {
				return err
			}
			cli := api.NewClient(cfg.APIListen)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if !cli.Reachable(ctx) {
				return fmt.Errorf("daemon not running on %s", cfg.APIListen)
			}
			out, err := os.Create(args[1])
			if err != nil {
				return err
			}
			defer out.Close()
			if err := cli.Download(ctx, args[0], out); err != nil {
				return err
			}
			st, _ := out.Stat()
			fmt.Printf("✓ downloaded → %s  (%d bytes)\n", args[1], st.Size())
			return nil
		},
	}
	return c
}

func cmdLs() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List files known to this node (across all groups)",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(flagDataDir)
			if err != nil {
				return err
			}
			cli := api.NewClient(cfg.APIListen)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if !cli.Reachable(ctx) {
				return fmt.Errorf("daemon not running on %s", cfg.APIListen)
			}
			resp, err := cli.ListFiles(ctx)
			if err != nil {
				return err
			}
			if len(resp.Files) == 0 {
				fmt.Println("(no files)")
				return nil
			}
			for _, f := range resp.Files {
				gid := "-"
				if f.GroupID != "" {
					gid = f.GroupID[:8]
				}
				fmt.Printf("%s  %s  %-7s  %10d B  %s\n",
					f.FileID[:16], gid, f.Mode, f.Size, f.Filename)
			}
			return nil
		},
	}
	return c
}

// ---------------- helpers ----------------

func openStore() (*store.Store, config.Config, error) {
	cfg, err := config.Load(flagDataDir)
	if err != nil {
		return nil, config.Config{}, err
	}
	s, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, config.Config{}, err
	}
	return s, cfg, nil
}

func statusFromDB(cfg config.Config) error {
	s, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer s.Close()
	id, err := keys.LoadOrCreate(filepath.Join(cfg.DataDir, "node.key"))
	if err != nil {
		return err
	}
	resp := &api.StatusResponse{
		PeerID: id.PeerID.String(), Nickname: cfg.Nickname,
		DataDir: cfg.DataDir, ContributedB: cfg.ContributedBytes,
	}
	groups, _ := group.ListGroups(s)
	for _, g := range groups {
		members, _ := group.ListMembers(s, g.ID)
		ms := make([]api.MemberSummary, 0, len(members))
		for _, m := range members {
			ms = append(ms, api.MemberSummary{
				PeerID: m.PeerID, Nickname: m.Nickname,
				ContributedB: m.ContributedB, JoinedAt: m.JoinedAt,
			})
		}
		resp.Groups = append(resp.Groups, api.GroupSummary{
			ID: g.ID, Name: g.Name, IsAdmin: g.IsAdminNode, Members: ms,
		})
	}
	printStatus(resp)
	return nil
}

func inviteFromDB(cfg config.Config, groupID string, bootstrap []string, ttlHours int, quotaGB int64) error {
	s, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer s.Close()

	if groupID == "" {
		groups, err := group.ListGroups(s)
		if err != nil {
			return err
		}
		if len(groups) != 1 {
			return fmt.Errorf("you are in %d groups — please pass --group <id>", len(groups))
		}
		groupID = groups[0].ID
	}
	g, err := group.LoadByID(s, groupID)
	if err != nil {
		return err
	}
	if !g.IsAdminNode {
		return fmt.Errorf("not the admin of group %s", groupID[:12])
	}
	ttl := time.Duration(ttlHours) * time.Hour
	var quota int64
	if quotaGB > 0 {
		quota = quotaGB * 1024 * 1024 * 1024
	}
	inv, err := invite.Create(g, bootstrap, ttl, quota)
	if err != nil {
		return err
	}
	link, err := inv.Encode()
	if err != nil {
		return err
	}
	fmt.Println(link)
	return nil
}

func printStatus(s *api.StatusResponse) {
	fmt.Printf("node    : %s (%s)\n", s.Nickname, s.PeerID)
	fmt.Printf("data    : %s\n", s.DataDir)
	fmt.Printf("quota   : %.1f GiB\n", float64(s.ContributedB)/(1024*1024*1024))
	if len(s.Groups) == 0 {
		fmt.Println("groups  : <none>")
		return
	}
	fmt.Printf("groups  : %d\n", len(s.Groups))
	for _, g := range s.Groups {
		role := "member"
		if g.IsAdmin {
			role = "admin"
		}
		fmt.Printf("  %s  %-20s  [%s]  %d member(s)\n",
			g.ID[:12], g.Name, role, len(g.Members))
		for _, m := range g.Members {
			fmt.Printf("    - %-20s  %s  %.1f GiB\n",
				m.Nickname, m.PeerID, float64(m.ContributedB)/(1024*1024*1024))
		}
	}
	if len(s.ConnectedPeers) > 0 {
		fmt.Printf("connected peers: %d\n", len(s.ConnectedPeers))
	}
}

func shortPeer(ids []string) string {
	if len(ids) == 0 {
		return "?"
	}
	id := ids[0]
	if len(id) > 12 {
		id = id[len(id)-12:]
	}
	return id
}
