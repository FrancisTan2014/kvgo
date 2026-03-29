package raft

import (
	"context"
	"kvgo/raftpb"
	"kvgo/transport"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
)

const (
	backoffMin = 50 * time.Millisecond
	backoffMax = 1 * time.Second
)

type tcpPeer struct {
	id   uint64
	addr string

	writeTimeout time.Duration

	msgc   chan *raftpb.Message // Send → writerLoop
	resetc chan struct{}        // acceptLoop → writerLoop (wake from backoff)
	stopc  chan struct{}
	ctx    context.Context
	cancel context.CancelFunc

	lg *slog.Logger

	stopOnce sync.Once
	wg       sync.WaitGroup
}

func newTCPPeer(id uint64, addr string, writeTimeout time.Duration, lg *slog.Logger) *tcpPeer {
	ctx, cancel := context.WithCancel(context.Background())
	p := &tcpPeer{
		id:           id,
		addr:         addr,
		writeTimeout: writeTimeout,
		msgc:         make(chan *raftpb.Message, messageBufferSize),
		resetc:       make(chan struct{}, 1),
		stopc:        make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
		lg:           lg,
	}
	p.wg.Add(1)
	go p.writerLoop()
	return p
}

func (p *tcpPeer) Send(msgs []*raftpb.Message) {
	for _, m := range msgs {
		select {
		case p.msgc <- m:
		default:
			p.lg.Warn("write buffer full, message dropped", "to", p.id)
		}
	}
}

func (p *tcpPeer) Stop() {
	p.stopOnce.Do(func() {
		p.cancel()
		close(p.stopc)
	})
	p.wg.Wait()
}

// Reset wakes the writerLoop from backoff so it re-dials immediately.
func (p *tcpPeer) Reset() {
	select {
	case p.resetc <- struct{}{}:
	default:
	}
}

func (p *tcpPeer) writerLoop() {
	defer p.wg.Done()
	var failures int

	for {
		select {
		case <-p.stopc:
			return
		default:
		}

		var d net.Dialer
		conn, err := d.DialContext(p.ctx, "tcp", p.addr)
		if err != nil {
			p.lg.Debug("dial failed", "peer", p.id, "addr", p.addr, "error", err)
			failures++
			if !p.backoff(failures) {
				return // stopped
			}
			continue
		}

		failures = 0
		f := transport.NewFramer(conn, conn)
		p.writeToConn(conn, f)
	}
}

func (p *tcpPeer) writeToConn(conn net.Conn, f *transport.Framer) {
	defer conn.Close()

	for {
		select {
		case <-p.stopc:
			return
		case m := <-p.msgc:
			data, err := proto.Marshal(m)
			if err != nil {
				p.lg.Warn("marshal failed", "peer", p.id, "error", err)
				continue
			}
			if p.writeTimeout > 0 {
				conn.SetWriteDeadline(time.Now().Add(p.writeTimeout))
			}
			if err := f.Write(data); err != nil {
				p.lg.Debug("write failed", "peer", p.id, "error", err)
				return
			}
		}
	}
}

// backoff waits with exponential delay + jitter. Returns false if stopped.
func (p *tcpPeer) backoff(failures int) bool {
	d := backoffDuration(failures)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-p.stopc:
		return false
	case <-p.resetc:
		return true
	case <-t.C:
		return true
	}
}

func backoffDuration(failures int) time.Duration {
	exp := float64(backoffMin) * math.Pow(2, float64(failures-1))
	if exp > float64(backoffMax) {
		exp = float64(backoffMax)
	}
	jitter := 1.0 + 0.25*(rand.Float64()*2-1) // ±25%
	d := time.Duration(exp * jitter)
	if d < backoffMin {
		d = backoffMin
	}
	if d > backoffMax {
		d = backoffMax
	}
	return d
}
