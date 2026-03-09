package raft

import "encoding/binary"

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
