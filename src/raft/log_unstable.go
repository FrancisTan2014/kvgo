package raft

import "kvgo/raftpb"

type unstable struct {
	entries []*raftpb.Entry
	offset  uint64
}

func (u *unstable) stableTo(index, term uint64) {
	if index < u.offset {
		return
	}
	i := index - u.offset
	if int(i) >= len(u.entries) {
		return
	}
	if u.entries[i].Term != term {
		return
	}
	u.entries = u.entries[i+1:]
	u.offset = index + 1
}

func (u *unstable) truncateAndAppend(entries []*raftpb.Entry) {
	first := entries[0].Index
	if first <= u.offset {
		u.offset = first
		u.entries = entries
	} else if first == u.offset+uint64(len(u.entries)) {
		u.entries = append(u.entries, entries...)
	} else {
		u.entries = u.entries[:first-u.offset]
		u.entries = append(u.entries, entries...)
	}
}

func (u *unstable) maybeLastIndex() (uint64, bool) {
	if len(u.entries) == 0 {
		return 0, false
	}
	return u.entries[len(u.entries)-1].Index, true
}

func (u *unstable) maybeTerm(index uint64) (uint64, bool) {
	if index < u.offset || index >= u.offset+uint64(len(u.entries)) {
		return 0, false
	}
	return u.entries[index-u.offset].Term, true
}
