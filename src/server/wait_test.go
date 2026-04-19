package server

import (
	"context"
	"kvgo/pkg/wait"
	"kvgo/raft"
	"kvgo/raftpb"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// --- envelope tests ---

func TestEnvelopeMarshalUnmarshal_036o(t *testing.T) {
	id := uint64(42)
	payload := []byte("set x 1")

	data := marshalEnvelope(id, payload)
	require.Equal(t, envelopeHeaderSize+len(payload), len(data))

	gotID, gotPayload, err := unmarshalEnvelope(data)
	require.NoError(t, err)
	require.Equal(t, id, gotID)
	require.Equal(t, payload, gotPayload)
}

func TestEnvelopeUnmarshalTooShort_036o(t *testing.T) {
	_, _, err := unmarshalEnvelope([]byte{1, 2, 3})
	require.ErrorIs(t, err, errEnvelopeTooShort)
}

// --- integration: propose → apply → trigger ---

func TestProposalSignaledAfterApply_036o(t *testing.T) {
	fn := &fakeNode{c: make(chan raft.Ready, 1)}
	cfg := newRaftHostConfig()
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fn,
	}
	host, err := newRaftHost(rc)
	require.NoError(t, err)
	host.Start()
	defer host.Stop()

	// server-side: register waiter, marshal envelope, propose
	w := wait.New()
	id := uint64(7)
	payload := []byte("set x 1")
	ch := w.Register(id)
	data := marshalEnvelope(id, payload)
	require.NoError(t, host.Propose(context.Background(), data))

	// simulate Raft committing the entry
	fn.c <- raft.Ready{
		CommittedEntries: []*raftpb.Entry{{Index: 1, Term: 1, Data: data}},
	}

	// server-side apply loop: read from channel, unmarshal, trigger
	select {
	case ap := <-host.Apply():
		for _, raw := range ap.data {
			gotID, gotPayload, err := unmarshalEnvelope(raw)
			require.NoError(t, err)
			require.Equal(t, id, gotID)
			require.Equal(t, payload, gotPayload)
			w.Trigger(gotID, "applied")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for apply channel")
	}

	// handler receives the result
	select {
	case v := <-ch:
		require.Equal(t, "applied", v)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for waiter signal")
	}
}

func TestProposalTimesOutWithoutCommit_036o(t *testing.T) {
	w := wait.New()
	id := uint64(8)
	ch := w.Register(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	// handler waits with timeout — no commit ever arrives
	select {
	case <-ch:
		t.Fatal("should not receive before timeout")
	case <-ctx.Done():
		// clean up the orphan waiter
		w.Trigger(id, nil)
	}

	// verify the waiter was removed — second trigger is a no-op
	w.Trigger(id, "stale")

	// drain the channel — should have nil from cleanup, not "stale"
	select {
	case v := <-ch:
		require.Nil(t, v)
	default:
		t.Fatal("expected nil result from cleanup trigger")
	}
}

func TestHostApplyChannelCarriesDataNotEntries_036o(t *testing.T) {
	fn := &fakeNode{c: make(chan raft.Ready, 1)}
	cfg := newRaftHostConfig()
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fn,
	}
	host, err := newRaftHost(rc)
	require.NoError(t, err)
	host.Start()
	defer host.Stop()

	fn.c <- raft.Ready{
		CommittedEntries: []*raftpb.Entry{
			{Index: 1, Term: 1, Data: []byte("a")},
			{Index: 2, Term: 1, Data: []byte("b")},
		},
	}

	select {
	case ap := <-host.Apply():
		// toApply carries [][]byte, not []raftpb.Entry
		require.Len(t, ap.data, 2)
		require.Equal(t, []byte("a"), ap.data[0])
		require.Equal(t, []byte("b"), ap.data[1])
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for apply channel")
	}
}
