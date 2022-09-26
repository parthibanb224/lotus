package sealer

import (
	"bufio"
	"context"
	"io"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"go.opencensus.io/stats"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/dagstore/mount"
	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/lotus/metrics"
	trace "go.opentelemetry.io/otel/trace"
)

var (
	Tracer trace.Tracer
)

// For small read skips, it's faster to "burn" some bytes than to setup new sector reader.
// Assuming 1ms stream seek latency, and 1G/s stream rate, we're willing to discard up to 1 MiB.
var MaxPieceReaderBurnBytes int64 = 1 << 20 // 1M
var ReadBuf = 128 * (127 * 8)               // unpadded(128k)

type pieceGetter func(ctx context.Context, offset uint64) (io.ReadCloser, error)

type pieceReader struct {
	ctx       context.Context
	name      string
	span      trace.Span
	getReader pieceGetter
	pieceCid  cid.Cid
	len       abi.UnpaddedPieceSize
	onClose   context.CancelFunc

	closed bool
	seqAt  int64 // next byte to be read by io.Reader

	mu  sync.Mutex
	r   io.ReadCloser
	br  *bufio.Reader
	rAt int64
}

func (p *pieceReader) init() (_ *pieceReader, err error) {
	stats.Record(p.ctx, metrics.DagStorePRInitCount.M(1))

	p.ctx, p.span = Tracer.Start(p.ctx, p.name)
	go func() {
		time.Sleep(4 * 60 * time.Second)
		p.span.End()
	}()

	p.rAt = 0
	p.r, err = p.getReader(p.ctx, uint64(p.rAt))
	if err != nil {
		return nil, err
	}
	if p.r == nil {
		return nil, nil
	}

	p.br = bufio.NewReaderSize(p.r, ReadBuf)

	return p, nil
}

func (p *pieceReader) check() error {
	if p.closed {
		return xerrors.Errorf("reader closed")
	}

	return nil
}

func (p *pieceReader) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.span.End()

	if err := p.check(); err != nil {
		return err
	}

	if p.r != nil {
		if err := p.r.Close(); err != nil {
			return err
		}
		if err := p.r.Close(); err != nil {
			return err
		}
		p.r = nil
	}

	p.onClose()

	p.closed = true

	return nil
}

func (p *pieceReader) Read(b []byte) (int, error) {
	start := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()

	var ctx context.Context
	var span trace.Span
	ctx, span = Tracer.Start(p.ctx, "pr.read")
	defer span.End()

	lockAcquireDuration := time.Since(start)

	if err := p.check(); err != nil {
		return 0, err
	}

	n, err := p.readAtUnlocked(ctx, b, p.seqAt, lockAcquireDuration)
	p.seqAt += int64(n)
	return n, err
}

func (p *pieceReader) Seek(offset int64, whence int) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.check(); err != nil {
		return 0, err
	}

	switch whence {
	case io.SeekStart:
		p.seqAt = offset
	case io.SeekCurrent:
		p.seqAt += offset
	case io.SeekEnd:
		p.seqAt = int64(p.len) + offset
	default:
		return 0, xerrors.Errorf("bad whence")
	}

	return p.seqAt, nil
}

func (p *pieceReader) ReadAt(b []byte, off int64) (n int, err error) {
	start := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	lockAcquireDuration := time.Since(start)

	var ctx context.Context
	var span trace.Span
	ctx, span = Tracer.Start(p.ctx, "pr.readAt")
	defer span.End()

	return p.readAtUnlocked(ctx, b, off, lockAcquireDuration)
}

func (p *pieceReader) readAtUnlocked(ctx context.Context, b []byte, off int64, lockAcqDuration time.Duration) (n int, err error) {
	start := time.Now()
	if err := p.check(); err != nil {
		return 0, err
	}

	stats.Record(p.ctx, metrics.DagStorePRBytesRequested.M(int64(len(b))))

	// 1. Get the backing reader into the correct position

	// if the backing reader is ahead of the offset we want, or more than
	//  MaxPieceReaderBurnBytes behind, reset the reader
	if p.r == nil || p.rAt > off || p.rAt+MaxPieceReaderBurnBytes < off {
		if p.r != nil {
			if err := p.r.Close(); err != nil {
				return 0, xerrors.Errorf("closing backing reader: %w", err)
			}
			p.r = nil
			p.br = nil
		}

		log.Debugw("pieceReader new stream", "piece", p.pieceCid, "at", p.rAt, "off", off-p.rAt, "n", len(b))

		if off > p.rAt {
			stats.Record(p.ctx, metrics.DagStorePRSeekForwardBytes.M(off-p.rAt), metrics.DagStorePRSeekForwardCount.M(1))
		} else {
			stats.Record(p.ctx, metrics.DagStorePRSeekBackBytes.M(p.rAt-off), metrics.DagStorePRSeekBackCount.M(1))
		}

		p.rAt = off
		p.r, err = p.getReader(ctx, uint64(p.rAt))
		p.br = bufio.NewReaderSize(p.r, ReadBuf)
		if err != nil {
			return 0, xerrors.Errorf("getting backing reader: %w", err)
		}
	}

	// 2. Check if we need to burn some bytes
	if off > p.rAt {
		stats.Record(p.ctx, metrics.DagStorePRBytesDiscarded.M(off-p.rAt), metrics.DagStorePRDiscardCount.M(1))

		log.Debugw("pieceReader discard", "discard", off-p.rAt, "piece", p.pieceCid)

		n, err := io.CopyN(io.Discard, p.br, off-p.rAt)
		p.rAt += n
		if err != nil {
			return 0, xerrors.Errorf("discarding read gap: %w", err)
		}
	}

	// 3. Sanity check
	if off != p.rAt {
		return 0, xerrors.Errorf("bad reader offset; requested %d; at %d", off, p.rAt)
	}

	var span trace.Span
	_, span = Tracer.Start(ctx, "pr.actual_read")

	// 4. Read!
	readStart := time.Now()
	n, err = io.ReadFull(p.br, b)
	if n < len(b) {
		log.Debugw("pieceReader short read", "piece", p.pieceCid, "at", p.rAt, "toEnd", int64(p.len)-p.rAt, "n", len(b), "read", n, "err", err)
	}
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}

	span.End()

	log.Debugw("pieceReader allreads",
		"piece", p.pieceCid, "at", p.rAt, "toEnd", int64(p.len)-p.rAt,
		"n", len(b), "read", n, "err", err,
		"total-duration-ms", time.Since(start).Milliseconds(),
		"read-duration-ms", time.Since(readStart).Milliseconds(),
		"lock-acquire-duration-ms", lockAcqDuration.Milliseconds())
	p.rAt += int64(n)
	return n, err
}

var _ mount.Reader = (*pieceReader)(nil)
