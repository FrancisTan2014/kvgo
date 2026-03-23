package raft

import (
	"encoding/binary"
	"kvgo/raftpb"
)

const SnapshotMetaBytes = 16
const HardStateBytes = 24

func entrySize(e *raftpb.Entry) int {
	return 8 + 8 + len(e.Data)
}

func encodeEntry(e *raftpb.Entry, buf []byte) {
	binary.LittleEndian.PutUint64(buf, e.Index)
	binary.LittleEndian.PutUint64(buf[8:], e.Term)
	copy(buf[16:], e.Data)
}

func DecodeEntry(data []byte) (*raftpb.Entry, error) {
	if len(data) < 16 {
		return nil, ErrUnexpectedBytes
	}

	return &raftpb.Entry{
		Index: binary.LittleEndian.Uint64(data),
		Term:  binary.LittleEndian.Uint64(data[8:]),
		Data:  data[16:],
	}, nil
}

func encodeHardState(h *raftpb.HardState, buf []byte) {
	binary.LittleEndian.PutUint64(buf[0:], h.Term)
	binary.LittleEndian.PutUint64(buf[8:], h.VotedFor)
	binary.LittleEndian.PutUint64(buf[16:], h.CommittedIndex)
}

func DecodeHardState(data []byte) (*raftpb.HardState, error) {
	if len(data) != HardStateBytes {
		return nil, ErrUnexpectedBytes
	}

	return &raftpb.HardState{
		Term:           binary.LittleEndian.Uint64(data[0:8]),
		VotedFor:       binary.LittleEndian.Uint64(data[8:16]),
		CommittedIndex: binary.LittleEndian.Uint64(data[16:]),
	}, nil
}

func encodeSnapshotMeta(s *raftpb.SnapshotMeta, buf []byte) {
	binary.LittleEndian.PutUint64(buf[0:], s.LastIncludedIndex)
	binary.LittleEndian.PutUint64(buf[8:], s.LastIncludedTerm)
}

func DecodeSnapshotMeta(data []byte) (*raftpb.SnapshotMeta, error) {
	if len(data) != SnapshotMetaBytes {
		return nil, ErrUnexpectedBytes
	}

	return &raftpb.SnapshotMeta{
		LastIncludedIndex: binary.LittleEndian.Uint64(data[0:8]),
		LastIncludedTerm:  binary.LittleEndian.Uint64(data[8:16]),
	}, nil
}

func encodeSaveBatch(hard *raftpb.HardState, entries []*raftpb.Entry) []byte {
	entryBytes := 0
	for _, e := range entries {
		entryBytes += frameHeaderSize + entrySize(e)
	}

	batchSize := HardStateBytes + frameHeaderSize + entryBytes
	buf := make([]byte, batchSize)
	encodeHardState(hard, buf)
	binary.LittleEndian.PutUint32(buf[HardStateBytes:], uint32(entryBytes))

	idx := HardStateBytes + frameHeaderSize
	for _, e := range entries {
		size := entrySize(e)
		binary.LittleEndian.PutUint32(buf[idx:], uint32(size))
		idx += frameHeaderSize
		encodeEntry(e, buf[idx:])
		idx += size
	}

	return buf
}

func decodeSaveBatch(batch []byte) (*raftpb.HardState, []*raftpb.Entry, error) {
	if len(batch) < HardStateBytes {
		return nil, nil, ErrCorruption
	}
	hd, _ := DecodeHardState(batch[0:HardStateBytes])

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

func decodeEntries(data []byte) ([]*raftpb.Entry, error) {
	entries := make([]*raftpb.Entry, 0)
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
