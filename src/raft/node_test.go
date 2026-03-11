package raft

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConsumeEntryAfterPropose(t *testing.T) {
	n := setupNode(1)
	n.r.state = Leader
	ctx := context.Background()
	defer ctx.Done()
	go n.run(ctx)

	require.NoError(t, n.Propose(ctx, []byte("foo")))
	r := <-n.Ready()
	require.Len(t, r.Entries, 1)
}

func TestAdvanceMovesForward(t *testing.T) {
	n := setupNode(1)
	n.r.state = Leader
	ctx := context.Background()
	defer ctx.Done()
	go n.run(ctx)

	require.NoError(t, n.Propose(ctx, []byte("foo")))
	r1 := <-n.Ready()
	require.Len(t, r1.Entries, 1)

	n.Advance()

	require.NoError(t, n.Propose(ctx, []byte("bar")))
	r2 := <-n.Ready()
	require.Len(t, r2.Entries, 1) // only "bar", not "foo"+"bar"
	require.Equal(t, []byte("bar"), r2.Entries[0].Data)
}
