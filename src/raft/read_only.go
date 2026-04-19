package raft

import "kvgo/raftpb"

type ReadState struct {
	Index      uint64
	RequestCtx []byte
}

type readIndexStatus struct {
	req   *raftpb.Message
	index uint64
	acks  map[uint64]bool
}

type readOnly struct {
	pendingReadIndex map[string]*readIndexStatus
	readIndexQueue   []string
}

func newReadOnly() *readOnly {
	return &readOnly{
		pendingReadIndex: make(map[string]*readIndexStatus),
	}
}

func (r *readOnly) addRequest(i uint64, m *raftpb.Message) {
	s := string(m.Context)
	if _, ok := r.pendingReadIndex[s]; ok {
		return
	}

	r.pendingReadIndex[s] = &readIndexStatus{req: m, index: i, acks: make(map[uint64]bool)}
	r.readIndexQueue = append(r.readIndexQueue, s)
}

func (r *readOnly) recvAck(id uint64, ctx []byte) map[uint64]bool {
	rs, ok := r.pendingReadIndex[string(ctx)]
	if !ok {
		return nil
	}

	rs.acks[id] = true
	return rs.acks
}

func (r *readOnly) advance(m *raftpb.Message) []*readIndexStatus {
	var (
		i     int
		found bool
	)

	ctx := string(m.Context)
	var rss []*readIndexStatus

	for _, okctx := range r.readIndexQueue {
		i++
		rs, ok := r.pendingReadIndex[okctx]
		if !ok {
			panic("cannot find corresponding read state from pending map")
		}
		rss = append(rss, rs)
		if okctx == ctx {
			found = true
			break
		}
	}

	if found {
		r.readIndexQueue = r.readIndexQueue[i:]
		for _, rs := range rss {
			delete(r.pendingReadIndex, string(rs.req.Context))
		}
		return rss
	}

	return nil
}
