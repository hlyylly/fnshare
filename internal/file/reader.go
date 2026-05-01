package file

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/fnshare/fnshare/internal/ec"
	"github.com/fnshare/fnshare/internal/manifest"

	"github.com/libp2p/go-libp2p/core/peer"
)

// Reader provides random + sequential access to a file's plaintext bytes,
// fetching + decrypting stripes on demand. Returned by Service.OpenReader.
//
// Implements io.Reader, io.ReaderAt, io.Seeker, io.Closer so it works
// equally well for FUSE (random ReadAt) and HTTP downloads (sequential
// io.Copy).
type Reader struct {
	svc *Service
	m   *manifest.Manifest
	key []byte

	pos int64 // current sequential read offset (for io.Reader / io.Seeker)

	decryptedFilename string
}

func (r *Reader) Manifest() *manifest.Manifest {
	// Return a shallow copy with the user-visible filename slot already
	// populated. Useful for FUSE Lookup so we don't have to redo the
	// decrypt to render a directory entry.
	mm := *r.m
	if r.decryptedFilename != "" {
		mm.Filename = r.decryptedFilename
	}
	return &mm
}

func (r *Reader) Size() int64 { return r.m.Size }

func (r *Reader) Close() error { return nil }

// Read serves bytes sequentially starting at the current position.
func (r *Reader) Read(dst []byte) (int, error) {
	if r.pos >= r.m.Size {
		return 0, io.EOF
	}
	n, err := r.ReadAt(dst, r.pos)
	r.pos += int64(n)
	return n, err
}

// Seek implements io.Seeker.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.pos = offset
	case io.SeekCurrent:
		r.pos += offset
	case io.SeekEnd:
		r.pos = r.m.Size + offset
	default:
		return 0, errors.New("file: bad whence")
	}
	if r.pos < 0 {
		r.pos = 0
	}
	return r.pos, nil
}

// ReadAt reads len(dst) bytes (or fewer if EOF) starting at offset. Only
// the stripes touching [offset, offset+len(dst)) are fetched + decrypted.
func (r *Reader) ReadAt(dst []byte, offset int64) (int, error) {
	if offset >= r.m.Size {
		return 0, io.EOF
	}
	end := offset + int64(len(dst))
	if end > r.m.Size {
		end = r.m.Size
	}
	written := 0
	stripeSize := int64(r.m.StripeDataSize)
	for cur := offset; cur < end; {
		stripeIdx := int(cur / stripeSize)
		if stripeIdx >= len(r.m.Stripes) {
			break
		}
		stripeStart := int64(stripeIdx) * stripeSize

		stripeData, err := r.stripeBytes(stripeIdx)
		if err != nil {
			return written, err
		}

		inStart := int(cur - stripeStart)
		want := int(end - cur)
		if avail := len(stripeData) - inStart; want > avail {
			want = avail
		}
		copy(dst[written:written+want], stripeData[inStart:inStart+want])
		written += want
		cur += int64(want)

		if want == 0 { // safety: shouldn't happen, but break to avoid spin
			break
		}
	}
	if written < len(dst) && offset+int64(written) >= r.m.Size {
		return written, io.EOF
	}
	return written, nil
}

// stripeBytes returns the decrypted plaintext for a stripe, hitting the
// shared cache first.
func (r *Reader) stripeBytes(idx int) ([]byte, error) {
	if data, ok := r.svc.cache.Get(r.m.FileID, idx); ok {
		return data, nil
	}
	data, err := r.fetchAndDecryptStripe(idx)
	if err != nil {
		return nil, err
	}
	r.svc.cache.Put(r.m.FileID, idx, data)
	return data, nil
}

func (r *Reader) fetchAndDecryptStripe(idx int) ([]byte, error) {
	if idx < 0 || idx >= len(r.m.Stripes) {
		return nil, fmt.Errorf("stripe index out of range: %d", idx)
	}
	stripe := r.m.Stripes[idx]

	total := r.m.DataShards + r.m.ParityShards
	if len(stripe.ShardIDs) != total || len(r.m.Holders) != total {
		return nil, fmt.Errorf("manifest stripe %d malformed", idx)
	}

	shardBytes := make([][]byte, total)
	gathered := 0
	ctx := context.Background()

	for i := 0; i < total && gathered < r.m.DataShards; i++ {
		shardID := stripe.ShardIDs[i]
		holder := r.m.Holders[i]

		// Local first.
		if data, err := r.svc.blockstore.Get(shardID); err == nil {
			shardBytes[i] = data
			gathered++
			continue
		}
		// Remote.
		if holder == r.svc.node.Host.ID().String() {
			continue // we'd already have it locally
		}
		pid, err := peer.Decode(holder)
		if err != nil {
			continue
		}
		data, err := r.svc.node.GetShardFromPeer(ctx, pid, shardID)
		if err != nil {
			r.svc.log.Debugw("fetch shard", "stripe", idx, "slot", i, "holder", holder, "err", err)
			continue
		}
		shardBytes[i] = data
		gathered++
	}
	if gathered < r.m.DataShards {
		return nil, fmt.Errorf("stripe %d: only %d of %d required shards available",
			idx, gathered, r.m.DataShards)
	}

	params := ec.Params{DataShards: r.m.DataShards, ParityShards: r.m.ParityShards}
	ct, err := params.Decode(shardBytes, int64(stripe.CiphertextBytes))
	if err != nil {
		return nil, fmt.Errorf("ec decode stripe %d: %w", idx, err)
	}
	pt, err := openStripeCiphertext(r.key, ct)
	if err != nil {
		return nil, fmt.Errorf("decrypt stripe %d: %w", idx, err)
	}
	return pt, nil
}

// openStripeCiphertext is just crypto.OpenAES (the stripe ciphertext is
// nonce(12) || GCM(plaintext), exactly what crypto.SealAES produces).
// Wrapped here for symmetry with future per-stripe key derivation.
func openStripeCiphertext(key, ct []byte) ([]byte, error) {
	return cryptoOpenAES(key, ct)
}
