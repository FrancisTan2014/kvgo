package server

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"kvgo/protocol"
	"kvgo/raft"
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
	snap       raft.SnapshotMeta
	applied    []raft.SnapshotMeta
	saved      []raft.Entry
	savedHard  raft.HardState
	saveErr    error
	savedCh    chan []raft.Entry
	events     chan string
}

func (m *mockStorage) InitialState() (raft.HardState, error) {
	return raft.HardState{}, nil
}

func (m *mockStorage) Save(entries []raft.Entry, hard raft.HardState) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.saved = entries
	m.savedHard = hard
	if m.savedCh != nil {
		saved := append([]raft.Entry(nil), entries...)
		m.savedCh <- saved
	}
	if m.events != nil {
		m.events <- "save"
	}
	return nil
}

func (m *mockStorage) Entries(lo, hi uint64) ([]raft.Entry, error) {
	return nil, nil
}

func (m *mockStorage) FirstIndex() uint64 {
	return m.firstIndex
}

func (m *mockStorage) LastIndex() uint64 {
	if m.snap.LastIncludedIndex > 0 {
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

func (m *mockStorage) Snapshot() (raft.SnapshotMeta, error) {
	return m.snap, nil
}

func (m *mockStorage) ApplySnapshot(snap raft.SnapshotMeta) error {
	m.snap = snap
	m.applied = append(m.applied, snap)
	return nil
}

type mockRaftTransport struct {
	sent   chan raft.Message
	events chan string
}

func (t *mockRaftTransport) Send(msgs []raft.Message) {
	for _, m := range msgs {
		if t.sent != nil {
			t.sent <- m
		}
		if t.events != nil {
			t.events <- "send"
		}
	}
}

type fakeNode struct {
	c           chan raft.Ready
	proposed    []byte
	proposeErr  error
	steppedMsg  *raft.Message
	stepErr     error
	campaigned  bool
	campaignErr error
	advanced    bool
	advancedCh  chan struct{}
	events      chan string
}

func (n *fakeNode) Ready() <-chan raft.Ready {
	return n.c
}

func (n *fakeNode) Propose(ctx context.Context, data []byte) error {
	n.proposed = append([]byte(nil), data...)
	return n.proposeErr
}

func (n *fakeNode) Step(ctx context.Context, m raft.Message) error {
	n.steppedMsg = &m
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

type fakeStateMachine struct {
	data map[string][]byte
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
	s.data[key] = append([]byte(nil), value...)
	return nil
}

type fakeRaftHost struct {
	applyc chan toApply
	errorc chan error
}

func newFakeRaftHost() *fakeRaftHost {
	return &fakeRaftHost{
		applyc: make(chan toApply),
		errorc: make(chan error),
	}
}

func (r *fakeRaftHost) Propose(ctx context.Context, data []byte) error {
	return nil
}

func (r *fakeRaftHost) Step(ctx context.Context, m raft.Message) error {
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
