package raft

import "encoding/binary"

const SnapshotMetaBytes = 16

type MessageType int32

const (
	MsgApp     MessageType = 1
	MsgAppResp MessageType = 2
)

type Message struct {
	Type    MessageType
	From    uint64
	To      uint64
	Index   uint64
	Term    uint64
	Entries []Entry
}

func (e *Entry) Size() int {
	return 8 + 8 + len(e.Data)
}

func (e *Entry) EncodeTo(buf []byte) {
	binary.LittleEndian.PutUint64(buf, e.Index)
	binary.LittleEndian.PutUint64(buf[8:], e.Term)
	copy(buf[16:], e.Data)
}

func DecodeEntry(data []byte) (Entry, error) {
	if len(data) < 16 {
		return Entry{}, ErrUnexpectedBytes
	}

	return Entry{
		Index: binary.LittleEndian.Uint64(data),
		Term:  binary.LittleEndian.Uint64(data[8:]),
		Data:  data[16:],
	}, nil
}

func (e *HardState) EncodeTo(buf []byte) {
	binary.LittleEndian.PutUint64(buf[0:], e.Term)
	binary.LittleEndian.PutUint64(buf[8:], e.VotedFor)
	binary.LittleEndian.PutUint64(buf[16:], e.CommittedIndex)
}

func DecodeHardState(data []byte) (HardState, error) {
	if len(data) != HardStateBytes {
		return HardState{}, ErrUnexpectedBytes
	}

	return HardState{
		Term:           binary.LittleEndian.Uint64(data[0:8]),
		VotedFor:       binary.LittleEndian.Uint64(data[8:16]),
		CommittedIndex: binary.LittleEndian.Uint64(data[16:]),
	}, nil
}

func (s *SnapshotMeta) EncodeTo(buf []byte) {
	binary.LittleEndian.PutUint64(buf[0:], s.LastIncludedIndex)
	binary.LittleEndian.PutUint64(buf[8:], s.LastIncludedTerm)
}

func DecodeSnapshotMeta(data []byte) (SnapshotMeta, error) {
	if len(data) != SnapshotMetaBytes {
		return SnapshotMeta{}, ErrUnexpectedBytes
	}

	return SnapshotMeta{
		LastIncludedIndex: binary.LittleEndian.Uint64(data[0:8]),
		LastIncludedTerm:  binary.LittleEndian.Uint64(data[8:16]),
	}, nil
}

func encodeSaveBatch(hard HardState, entries []Entry) []byte {
	entryBytes := 0
	for _, e := range entries {
		entryBytes += frameHeaderSize + e.Size()
	}

	batchSize := HardStateBytes + frameHeaderSize + entryBytes
	buf := make([]byte, batchSize)
	hard.EncodeTo(buf)
	binary.LittleEndian.PutUint32(buf[HardStateBytes:], uint32(entryBytes))

	idx := HardStateBytes + frameHeaderSize
	for _, e := range entries {
		size := e.Size()
		binary.LittleEndian.PutUint32(buf[idx:], uint32(size))
		idx += frameHeaderSize
		e.EncodeTo(buf[idx:])
		idx += size
	}

	return buf
}

func decodeSaveBatch(batch []byte) (HardState, []Entry, error) {
	var hd HardState

	if len(batch) < HardStateBytes {
		return hd, nil, ErrCorruption
	}
	hd, _ = DecodeHardState(batch[0:HardStateBytes])

	batch = batch[HardStateBytes:]
	if len(batch) < frameHeaderSize {
		return hd, nil, ErrCorruption
	}

	entryBytes := int(binary.LittleEndian.Uint32(batch))
	batch = batch[frameHeaderSize:]
	if len(batch) != entryBytes {
		return hd, nil, ErrCorruption
	}

	ents, err := decodeEntries(batch)
	if err != nil {
		return hd, nil, ErrCorruption
	}

	return hd, ents, nil
}

func decodeEntries(data []byte) ([]Entry, error) {
	entries := make([]Entry, 0)
	idx := 0
	for {
		buf := data[idx:]
		if len(buf) == 0 {
			break
		}
		if len(buf) < frameHeaderSize {
			return nil, ErrCorruption
		}

		frame := int(binary.LittleEndian.Uint32(buf[0:frameHeaderSize]))
		idx += frameHeaderSize
		buf = data[idx:]
		if len(buf) < frame {
			return nil, ErrCorruption
		}

		e, err := DecodeEntry(buf[:frame])
		if err != nil {
			return nil, ErrCorruption
		}

		entries = append(entries, e)
		idx += frame
	}

	return entries, nil
}
