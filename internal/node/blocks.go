package node

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/fnshare/fnshare/internal/blockstore"
	"github.com/fnshare/fnshare/internal/manifest"

	"github.com/fxamacker/cbor/v2"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

const (
	ProtoBlocks protocol.ID = "/fnshare/blocks/1.0.0"

	blockOpTimeout = 60 * time.Second
	maxFrameSize   = 64 * 1024 * 1024 // 64 MiB cap per shard frame
)

// Op is the verb of a blocks-protocol request.
type Op uint8

const (
	OpPutShard Op = iota + 1
	OpGetShard
	OpPutManifest
	OpGetManifest
)

type Request struct {
	Op   Op     `cbor:"op"`
	ID   string `cbor:"id"`             // shard id (hex) or file id (hex)
	Body []byte `cbor:"body,omitempty"` // shard bytes or manifest CBOR (for PUT)
}

type Response struct {
	OK    bool   `cbor:"ok"`
	Error string `cbor:"err,omitempty"`
	Body  []byte `cbor:"body,omitempty"`
}

// AttachBlockstore wires the local block + manifest storage into the node so
// the protocol handler can serve and accept shards. Must be called before
// the daemon starts accepting traffic.
func (n *Node) AttachBlockstore(bs *blockstore.Store) {
	n.mu.Lock()
	n.blocks = bs
	n.mu.Unlock()
	n.Host.SetStreamHandler(ProtoBlocks, n.handleBlocks)
}

// ---------- server side ----------

func (n *Node) handleBlocks(s network.Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(blockOpTimeout))

	req, err := readFrame[Request](s)
	if err != nil {
		_ = writeFrame(s, Response{OK: false, Error: "decode: " + err.Error()})
		return
	}

	resp := n.serveBlock(req)

	// Account traffic against the remote peer (inbound side).
	if l := n.Ledger(); l != nil && resp.OK {
		remote := s.Conn().RemotePeer().String()
		switch req.Op {
		case OpPutShard:
			l.RecordStoredForThem(remote, int64(len(req.Body)))
		case OpGetShard:
			l.RecordServedToThem(remote, int64(len(resp.Body)))
		}
	}

	if err := writeFrame(s, resp); err != nil {
		n.Log.Warnw("blocks: write response", "err", err)
	}
}

func (n *Node) serveBlock(req *Request) Response {
	bs := n.Blocks()
	if bs == nil {
		return Response{OK: false, Error: "blockstore not initialized"}
	}
	switch req.Op {
	case OpPutShard:
		if err := bs.Put(req.ID, req.Body); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}

	case OpGetShard:
		raw, err := bs.Get(req.ID)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Body: raw}

	case OpPutManifest:
		m, err := manifest.Unmarshal(req.Body)
		if err != nil {
			return Response{OK: false, Error: "manifest decode: " + err.Error()}
		}
		if m.FileID != req.ID {
			return Response{OK: false, Error: "file id mismatch"}
		}
		if err := manifest.Put(n.Store, m); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}

	case OpGetManifest:
		m, err := manifest.Get(n.Store, req.ID)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		raw, err := manifest.Marshal(m)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Body: raw}

	default:
		return Response{OK: false, Error: fmt.Sprintf("unknown op %d", req.Op)}
	}
}

// ---------- client side ----------

func (n *Node) PutShardOnPeer(ctx context.Context, p peer.ID, shardID string, data []byte) error {
	resp, err := n.blockRequest(ctx, p, &Request{Op: OpPutShard, ID: shardID, Body: data})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	if l := n.Ledger(); l != nil {
		l.RecordStoredOnThem(p.String(), int64(len(data)))
	}
	return nil
}

func (n *Node) GetShardFromPeer(ctx context.Context, p peer.ID, shardID string) ([]byte, error) {
	resp, err := n.blockRequest(ctx, p, &Request{Op: OpGetShard, ID: shardID})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	if l := n.Ledger(); l != nil {
		l.RecordDownloadedFrom(p.String(), int64(len(resp.Body)))
	}
	return resp.Body, nil
}

func (n *Node) PutManifestOnPeer(ctx context.Context, p peer.ID, m *manifest.Manifest) error {
	raw, err := manifest.Marshal(m)
	if err != nil {
		return err
	}
	return n.PutManifestRawOnPeer(ctx, p, m.FileID, raw)
}

// PutManifestRawOnPeer ships an already-marshaled manifest. Used by the
// spool worker so we don't repeatedly unmarshal/remarshal on retry.
func (n *Node) PutManifestRawOnPeer(ctx context.Context, p peer.ID, fileID string, raw []byte) error {
	resp, err := n.blockRequest(ctx, p, &Request{Op: OpPutManifest, ID: fileID, Body: raw})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}

func (n *Node) GetManifestFromPeer(ctx context.Context, p peer.ID, fileID string) (*manifest.Manifest, error) {
	resp, err := n.blockRequest(ctx, p, &Request{Op: OpGetManifest, ID: fileID})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	return manifest.Unmarshal(resp.Body)
}

func (n *Node) blockRequest(ctx context.Context, p peer.ID, req *Request) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, blockOpTimeout)
	defer cancel()

	s, err := n.Host.NewStream(ctx, p, ProtoBlocks)
	if err != nil {
		return nil, fmt.Errorf("open blocks stream to %s: %w", p, err)
	}
	defer s.Close()

	if err := writeFrame(s, *req); err != nil {
		return nil, err
	}
	if err := s.CloseWrite(); err != nil {
		return nil, err
	}
	return readFrame[Response](s)
}

// ---------- length-prefixed CBOR framing ----------
// Wire format: 4-byte big-endian frame length, then CBOR payload.
// Single-message-per-direction; the stream is closed after the response.

func writeFrame(w io.Writer, v any) error {
	raw, err := cbor.Marshal(v)
	if err != nil {
		return err
	}
	if len(raw) > maxFrameSize {
		return fmt.Errorf("frame too large: %d > %d", len(raw), maxFrameSize)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(raw)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}

func readFrame[T any](r io.Reader) (*T, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameSize {
		return nil, fmt.Errorf("frame too large: %d > %d", n, maxFrameSize)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var v T
	if err := cbor.Unmarshal(buf, &v); err != nil {
		return nil, err
	}
	return &v, nil
}
