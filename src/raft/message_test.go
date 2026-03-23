package raft

import (
	"encoding/binary"
	"testing"

	"kvgo/raftpb"

	"github.com/stretchr/testify/require"
)

func TestEntryEncodeDecodeRoundTrip_036c(t *testing.T) {
	original := raftpb.Entry{Index: 7, Term: 3, Data: []byte("hello")}

	buf := make([]byte, entrySize(&original))
	encodeEntry(&original, buf)

	decoded, err := DecodeEntry(buf)
	require.NoError(t, err)
	require.Equal(t, &original, decoded)
}

func TestDecodeEntryRejectsShortBuffer_036c(t *testing.T) {
	_, err := DecodeEntry(make([]byte, 15))
	require.ErrorIs(t, err, ErrUnexpectedBytes)
}

func TestHardStateEncodeDecodeRoundTrip_036c(t *testing.T) {
	original := raftpb.HardState{Term: 4, VotedFor: 2, CommittedIndex: 9}

	buf := make([]byte, HardStateBytes)
	encodeHardState(&original, buf)

	decoded, err := DecodeHardState(buf)
	require.NoError(t, err)
	require.Equal(t, &original, decoded)
}

func TestDecodeHardStateRejectsWrongSize_036c(t *testing.T) {
	_, err := DecodeHardState(make([]byte, HardStateBytes-1))
	require.ErrorIs(t, err, ErrUnexpectedBytes)
}

func TestDecodeSaveBatchRoundTrip_036c(t *testing.T) {
	hard := &raftpb.HardState{Term: 2, VotedFor: 1, CommittedIndex: 3}
	entries := []*raftpb.Entry{
		{Index: 2, Term: 2, Data: []byte("two")},
		{Index: 3, Term: 2, Data: []byte("three")},
	}

	batch := encodeSaveBatch(hard, entries)

	decodedHard, decodedEntries, err := decodeSaveBatch(batch)
	require.NoError(t, err)
	require.Equal(t, hard, decodedHard)
	require.Equal(t, entries, decodedEntries)
}

func TestDecodeSaveBatchRejectsMismatchedEntryLength_036c(t *testing.T) {
	hard := &raftpb.HardState{Term: 1, VotedFor: 1, CommittedIndex: 1}
	entries := []*raftpb.Entry{{Index: 1, Term: 1, Data: []byte("one")}}
	batch := encodeSaveBatch(hard, entries)

	binary.LittleEndian.PutUint32(batch[HardStateBytes:], uint32(len(batch)))

	_, _, err := decodeSaveBatch(batch)
	require.ErrorIs(t, err, ErrCorruption)
}

func TestDecodeEntriesRejectsTruncatedFrame_036c(t *testing.T) {
	entry := raftpb.Entry{Index: 1, Term: 1, Data: []byte("one")}
	frame := make([]byte, frameHeaderSize+entrySize(&entry)-1)
	binary.LittleEndian.PutUint32(frame[:frameHeaderSize], uint32(entrySize(&entry)))
	encodeEntry(&entry, frame[frameHeaderSize:])

	_, err := decodeEntries(frame)
	require.ErrorIs(t, err, ErrCorruption)
}
