package raft

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSegmentRotationOnSizeLimit_037f(t *testing.T) {
	dir := t.TempDir()
	// Small limit so a single batch triggers rotation.
	sw, err := newSegmentedWAL(dir, 50)
	require.NoError(t, err)
	defer func() { require.NoError(t, sw.close()) }()

	require.Equal(t, 1, sw.segmentCount())
	require.Equal(t, segmentName(0), sw.currentSegment())

	// Write a batch larger than 50 bytes.
	data := make([]byte, 60)
	require.NoError(t, sw.write(data))
	require.NoError(t, sw.sync())
	require.NoError(t, sw.maybeCut())

	require.Equal(t, 2, sw.segmentCount())
	require.Equal(t, segmentName(1), sw.currentSegment())

	// Subsequent write goes to new segment.
	require.NoError(t, sw.write([]byte("hello")))
	require.NoError(t, sw.sync())

	// Verify the new segment file exists and has data.
	info, err := os.Stat(filepath.Join(dir, segmentName(1)))
	require.NoError(t, err)
	require.Greater(t, info.Size(), int64(0))
}

func TestReplayStitchesMultipleSegments_037f(t *testing.T) {
	dir := t.TempDir()
	// Small limit to force rotation.
	sw, err := newSegmentedWAL(dir, 50)
	require.NoError(t, err)

	batches := []string{"alpha", "bravo", "charlie"}
	for _, b := range batches {
		require.NoError(t, sw.write([]byte(b)))
		require.NoError(t, sw.sync())
		require.NoError(t, sw.maybeCut())
	}
	require.NoError(t, sw.close())

	// Reopen and read all.
	sw2, err := newSegmentedWAL(dir, 50)
	require.NoError(t, err)
	defer func() { require.NoError(t, sw2.close()) }()

	var recovered []string
	err = sw2.readAll(func(seg string, batch []byte) error {
		recovered = append(recovered, string(batch))
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, batches, recovered)
}

func TestDeleteSegmentsRemovesFiles_037f(t *testing.T) {
	dir := t.TempDir()
	sw, err := newSegmentedWAL(dir, 50)
	require.NoError(t, err)

	// Create 3 segments by writing and cutting.
	require.NoError(t, sw.write(make([]byte, 60)))
	require.NoError(t, sw.sync())
	require.NoError(t, sw.cut())

	require.NoError(t, sw.write(make([]byte, 60)))
	require.NoError(t, sw.sync())
	require.NoError(t, sw.cut())

	require.NoError(t, sw.write([]byte("current")))
	require.NoError(t, sw.sync())

	require.Equal(t, 3, sw.segmentCount())

	// Delete first segment.
	err = sw.deleteSegments(func(name string) bool {
		return name == segmentName(0)
	})
	require.NoError(t, err)
	require.Equal(t, 2, sw.segmentCount())

	// File should be gone.
	_, err = os.Stat(filepath.Join(dir, segmentName(0)))
	require.True(t, os.IsNotExist(err))

	// Current segment is never deleted.
	err = sw.deleteSegments(func(name string) bool { return true })
	require.NoError(t, err)
	require.Equal(t, 1, sw.segmentCount()) // only current remains
	require.NoError(t, sw.close())
}
