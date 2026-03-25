package server

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"kvgo/protocol"
	"kvgo/raft"
	"kvgo/raftpb"
)

// mockStreamTransport for testing - captures writes and provides configurable behavior
type mockStreamTransport struct {
	mu            sync.Mutex
	written       []byte
	shouldACK     bool
	sendNACK      bool
	delay         time.Duration
	lastRequestId string
	receiveData   []byte
	receiveErr    error
	address       string
}

func (m *mockStreamTransport) Send(ctx context.Context, payload []byte) error {
	if m.delay > 0 {
		time.Sleep(m.delay)
	}

	m.mu.Lock()
	m.written = payload

	// Capture RequestID for ACK/NACK
	if req, err := protocol.DecodeRequest(payload); err == nil && req.RequestId != "" {
		m.lastRequestId = req.RequestId
	}
	m.mu.Unlock()

	return nil
}

func (m *mockStreamTransport) Receive(ctx context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.receiveErr != nil {
		return nil, m.receiveErr
	}
	if m.receiveData != nil {
		return m.receiveData, nil
	}
	return nil, io.EOF
}

func (m *mockStreamTransport) Close() error {
	return nil
}

func (m *mockStreamTransport) RemoteAddr() string {
	if m.address != "" {
		return m.address
	}
	return "mock:6379"
}

// mockRequestTransport for testing request-response patterns
type mockRequestTransport struct {
	mu       sync.Mutex
	response []byte
	err      error
	delay    time.Duration
	address  string
}

func (m *mockRequestTransport) Request(ctx context.Context, payload []byte) ([]byte, error) {
	m.mu.Lock()
	delay := m.delay
	m.mu.Unlock()

	if delay > 0 {
		deadline, hasDeadline := ctx.Deadline()
		if hasDeadline && time.Until(deadline) < delay {
			return nil, errors.New("timeout")
		}
		time.Sleep(delay)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func (m *mockRequestTransport) Close() error {
	return nil
}

func (m *mockRequestTransport) RemoteAddr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.address != "" {
		return m.address
	}
	return "mock:1234"
}

// fakeStreamTransport - minimal implementation for pointer identity tests
type fakeStreamTransport struct {
	id int // Make non-zero-sized so each instance gets unique address
}

func (f *fakeStreamTransport) Send(ctx context.Context, payload []byte) error { return nil }
func (f *fakeStreamTransport) Receive(ctx context.Context) ([]byte, error)    { return nil, nil }
func (f *fakeStreamTransport) Close() error                                   { return nil }
func (f *fakeStreamTransport) RemoteAddr() string                             { return "fake:1234" }

type mockStorage struct {
	firstIndex uint64
	snap       *raftpb.SnapshotMeta
	applied    []*raftpb.SnapshotMeta
	saved      []*raftpb.Entry
	savedHard  *raftpb.HardState
	saveErr    error
	savedCh    chan []*raftpb.Entry
	events     chan string
}

func (m *mockStorage) InitialState() (*raftpb.HardState, error) {
	return &raftpb.HardState{}, nil
}

func (m *mockStorage) Save(entries []*raftpb.Entry, hard *raftpb.HardState) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.saved = entries
	m.savedHard = hard
	if m.savedCh != nil {
		saved := append([]*raftpb.Entry(nil), entries...)
		m.savedCh <- saved
	}
	if m.events != nil {
		m.events <- "save"
	}
	return nil
}

func (m *mockStorage) Entries(lo, hi uint64) ([]*raftpb.Entry, error) {
	return nil, nil
}

func (m *mockStorage) FirstIndex() uint64 {
	return m.firstIndex
}

func (m *mockStorage) LastIndex() uint64 {
	if m.snap != nil && m.snap.LastIncludedIndex > 0 {
		return m.snap.LastIncludedIndex
	}
	if m.firstIndex > 0 {
		return m.firstIndex - 1
	}
	return 0
}

func (m *mockStorage) Compact(index uint64) error {
	return nil
}

func (m *mockStorage) Close() error {
	return nil
}

func (m *mockStorage) Snapshot() (*raftpb.SnapshotMeta, error) {
	if m.snap == nil {
		return &raftpb.SnapshotMeta{}, nil
	}
	return m.snap, nil
}

func (m *mockStorage) ApplySnapshot(snap *raftpb.SnapshotMeta) error {
	m.snap = snap
	m.applied = append(m.applied, snap)
	return nil
}

type mockRaftTransport struct {
	sent   chan *raftpb.Message
	events chan string
}

func (t *mockRaftTransport) Send(msgs []*raftpb.Message) {
	for _, m := range msgs {
		if t.sent != nil {
			t.sent <- m
		}
		if t.events != nil {
			t.events <- "send"
		}
	}
}

func (r *mockRaftTransport) Start() error                   { return nil }
func (r *mockRaftTransport) Stop()                          {}
func (r *mockRaftTransport) Addr() net.Addr                 { return nil }
func (r *mockRaftTransport) AddPeer(id uint64, addr string) {}

type fakeNode struct {
	c           chan raft.Ready
	proposed    []byte
	proposeErr  error
	steppedMsg  *raftpb.Message
	stepErr     error
	campaigned  bool
	campaignErr error
	advanced    bool
	advancedCh  chan struct{}
	events      chan string
	stopped     bool
}

func (n *fakeNode) Ready() <-chan raft.Ready {
	return n.c
}

func (n *fakeNode) Propose(ctx context.Context, data []byte) error {
	n.proposed = append([]byte(nil), data...)
	return n.proposeErr
}

func (n *fakeNode) Step(ctx context.Context, m *raftpb.Message) error {
	n.steppedMsg = m
	return n.stepErr
}

func (n *fakeNode) Campaign(ctx context.Context) error {
	n.campaigned = true
	return n.campaignErr
}

func (n *fakeNode) Advance() {
	n.advanced = true
	if n.advancedCh != nil {
		n.advancedCh <- struct{}{}
	}
	if n.events != nil {
		n.events <- "advance"
	}
}

func (n *fakeNode) Tick() {}

func (n *fakeNode) Stop() { n.stopped = true }

type fakeStateMachine struct {
	data   map[string][]byte
	putErr error
}

func newFakeStateMachine() *fakeStateMachine {
	return &fakeStateMachine{
		data: make(map[string][]byte),
	}
}

func (s *fakeStateMachine) Get(key string) ([]byte, bool) {
	v, ok := s.data[key]
	return v, ok
}

func (s *fakeStateMachine) Put(key string, value []byte) error {
	if s.putErr != nil {
		return s.putErr
	}
	s.data[key] = append([]byte(nil), value...)
	return nil
}

type fakeRaftHost struct {
	applyc     chan toApply
	errorc     chan error
	proposeErr error
	proposed   []byte
	proposec   chan []byte
}

func newFakeRaftHost() *fakeRaftHost {
	return &fakeRaftHost{
		applyc:   make(chan toApply),
		errorc:   make(chan error),
		proposec: make(chan []byte, 1),
	}
}

func (r *fakeRaftHost) Propose(ctx context.Context, data []byte) error {
	if r.proposeErr != nil {
		return r.proposeErr
	}
	r.proposed = data
	select {
	case r.proposec <- data:
	default:
	}
	return nil
}

func (r *fakeRaftHost) Step(ctx context.Context, m *raftpb.Message) error {
	return nil
}

func (r *fakeRaftHost) Campaign(ctx context.Context) error {
	return nil
}

func (r *fakeRaftHost) Apply() <-chan toApply {
	return r.applyc
}

func (r *fakeRaftHost) Start() {}

func (r *fakeRaftHost) Stop() {}

func (r *fakeRaftHost) Errors() <-chan error {
	return r.errorc
}

func (r *fakeRaftHost) LeaderID() uint64 {
	return 0
}

type fakeWait struct {
	m map[uint64]chan any
}

func newFakeWait() *fakeWait {
	return &fakeWait{m: make(map[uint64]chan any)}
}

func (w *fakeWait) Register(id uint64) <-chan any {
	ch := make(chan any, 1)
	w.m[id] = ch
	return ch
}

func (w *fakeWait) Trigger(id uint64, x any) {
	ch, ok := w.m[id]
	if ok {
		delete(w.m, id)
		ch <- x
	}
}

func (w *fakeWait) IsRegistered(id uint64) bool {
	_, ok := w.m[id]
	return ok
}
