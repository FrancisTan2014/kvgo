package raft

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEntryEncodeDecodeRoundTrip(t *testing.T) {
	original := Entry{Index: 7, Term: 3, Data: []byte("hello")}

	buf := make([]byte, original.Size())
	original.EncodeTo(buf)

	decoded, err := DecodeEntry(buf)
	require.NoError(t, err)
	require.Equal(t, original, decoded)
}

func TestDecodeEntryRejectsShortBuffer(t *testing.T) {
	_, err := DecodeEntry(make([]byte, 15))
	require.ErrorIs(t, err, ErrUnexpectedBytes)
}

func TestHardStateEncodeDecodeRoundTrip(t *testing.T) {
	original := HardState{Term: 4, VotedFor: 2, CommittedIndex: 9}

	buf := make([]byte, HardStateBytes)
	original.EncodeTo(buf)

	decoded, err := DecodeHardState(buf)
	require.NoError(t, err)
	require.Equal(t, original, decoded)
}

func TestDecodeHardStateRejectsWrongSize(t *testing.T) {
	_, err := DecodeHardState(make([]byte, HardStateBytes-1))
	require.ErrorIs(t, err, ErrUnexpectedBytes)
}

func TestDecodeSaveBatchRoundTrip(t *testing.T) {
	hard := HardState{Term: 2, VotedFor: 1, CommittedIndex: 3}
	entries := []Entry{
		{Index: 2, Term: 2, Data: []byte("two")},
		{Index: 3, Term: 2, Data: []byte("three")},
	}

	batch := encodeSaveBatch(hard, entries)

	decodedHard, decodedEntries, err := decodeSaveBatch(batch)
	require.NoError(t, err)
	require.Equal(t, hard, decodedHard)
	require.Equal(t, entries, decodedEntries)
}

func TestDecodeSaveBatchRejectsMismatchedEntryLength(t *testing.T) {
	hard := HardState{Term: 1, VotedFor: 1, CommittedIndex: 1}
	entries := []Entry{{Index: 1, Term: 1, Data: []byte("one")}}
	batch := encodeSaveBatch(hard, entries)

	binary.LittleEndian.PutUint32(batch[HardStateBytes:], uint32(len(batch)))

	_, _, err := decodeSaveBatch(batch)
	require.ErrorIs(t, err, ErrCorruption)
}

func TestDecodeEntriesRejectsTruncatedFrame(t *testing.T) {
	entry := Entry{Index: 1, Term: 1, Data: []byte("one")}
	frame := make([]byte, frameHeaderSize+entry.Size()-1)
	binary.LittleEndian.PutUint32(frame[:frameHeaderSize], uint32(entry.Size()))
	entry.EncodeTo(frame[frameHeaderSize:])

	_, err := decodeEntries(frame)
	require.ErrorIs(t, err, ErrCorruption)
}
