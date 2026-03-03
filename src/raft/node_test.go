package raft

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConsumeEntryAfterPropose(t *testing.T) {
	n := setupNode(1)
	ctx := context.Background()
	go n.run(ctx)

	n.Propose(ctx, []byte("foo"))
	r := <-n.Ready()
	require.Len(t, r.Entries, 1)
}

func TestAdvanceMovesForward(t *testing.T) {
	n := setupNode(1)
	ctx := context.Background()
	go n.run(ctx)

	n.Propose(ctx, []byte("foo"))
	r1 := <-n.Ready()
	require.Len(t, r1.Entries, 1)

	n.Advance()

	n.Propose(ctx, []byte("bar"))
	r2 := <-n.Ready()
	require.Len(t, r2.Entries, 1) // only "bar", not "foo"+"bar"
	require.Equal(t, []byte("bar"), r2.Entries[0].Data)
}
