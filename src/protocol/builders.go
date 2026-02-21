package protocol

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Replicate (CmdReplicate)
// Value = "replid\nlistenAddr\nnodeID"
// ---------------------------------------------------------------------------

// ReplicateValue holds the parsed fields from a CmdReplicate request.
type ReplicateValue struct {
	Replid     string
	ListenAddr string
	NodeID     string
}

// NewReplicateRequest builds a CmdReplicate request.
func NewReplicateRequest(replid, listenAddr, nodeID string, lastSeq uint64) Request {
	val := replid + Delimiter + listenAddr + Delimiter + nodeID
	return Request{
		Cmd:   CmdReplicate,
		Seq:   lastSeq,
		Value: []byte(val),
	}
}

// ParseReplicateValue parses the Value field of a CmdReplicate request.
func ParseReplicateValue(value []byte) (ReplicateValue, error) {
	parts := strings.Split(string(value), Delimiter)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ReplicateValue{}, fmt.Errorf("expected replid, listenAddr, nodeID separated by delimiter")
	}
	return ReplicateValue{
		Replid:     parts[0],
		ListenAddr: parts[1],
		NodeID:     parts[2],
	}, nil
}

// ---------------------------------------------------------------------------
// Ping / Pong (CmdPing, StatusPong)
// Value = 8-byte LE term
// ---------------------------------------------------------------------------

// NewPingRequest builds a CmdPing request with the current seq and term.
func NewPingRequest(seq uint64, term uint64) Request {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, term)
	return Request{Cmd: CmdPing, Seq: seq, Value: buf}
}

// ParsePingTerm extracts the term from a CmdPing request Value.
func ParsePingTerm(value []byte) uint64 {
	if len(value) >= 8 {
		return binary.LittleEndian.Uint64(value[:8])
	}
	return 0
}

// NewPongResponse builds a StatusPong response carrying the node's term.
func NewPongResponse(term uint64) Response {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, term)
	return Response{Status: StatusPong, Value: buf}
}

// ParsePongTerm extracts the term from a StatusPong response Value.
func ParsePongTerm(value []byte) uint64 {
	if len(value) >= 8 {
		return binary.LittleEndian.Uint64(value[:8])
	}
	return 0
}

// ---------------------------------------------------------------------------
// Vote (CmdVoteRequest, StatusVoteResponse)
// Request Value = "term\nnodeID\nlastSeq"
// Response Value = [term u64 LE][granted u8]
// ---------------------------------------------------------------------------

// VoteRequestValue holds the parsed fields from a CmdVoteRequest.
type VoteRequestValue struct {
	Term    uint64
	NodeID  string
	LastSeq uint64
}

// NewVoteRequest builds a CmdVoteRequest.
func NewVoteRequest(term uint64, nodeID string, lastSeq uint64) Request {
	payload := fmt.Sprintf("%d%s%s%s%d", term, Delimiter, nodeID, Delimiter, lastSeq)
	return Request{
		Cmd:   CmdVoteRequest,
		Value: []byte(payload),
	}
}

// ParseVoteRequestValue parses the Value field of a CmdVoteRequest.
func ParseVoteRequestValue(value []byte) (VoteRequestValue, error) {
	parts := strings.Split(string(value), Delimiter)
	if len(parts) != 3 {
		return VoteRequestValue{}, fmt.Errorf("expected 3 fields, got %d", len(parts))
	}

	term, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return VoteRequestValue{}, fmt.Errorf("invalid term: %w", err)
	}

	lastSeq, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return VoteRequestValue{}, fmt.Errorf("invalid lastSeq: %w", err)
	}

	return VoteRequestValue{Term: term, NodeID: parts[1], LastSeq: lastSeq}, nil
}

// VoteResponseValue holds the parsed fields from a StatusVoteResponse.
type VoteResponseValue struct {
	Term    uint64
	Granted bool
}

// NewVoteResponse builds a StatusVoteResponse with the node's term and vote decision.
func NewVoteResponse(term uint64, granted bool) Response {
	buf := make([]byte, 9)
	binary.LittleEndian.PutUint64(buf, term)
	if granted {
		buf[8] = 1
	}
	return Response{Status: StatusVoteResponse, Value: buf}
}

// ParseVoteResponseValue parses the Value field of a StatusVoteResponse.
func ParseVoteResponseValue(value []byte) (VoteResponseValue, error) {
	if len(value) != 9 {
		return VoteResponseValue{}, fmt.Errorf("expected 9 bytes, got %d", len(value))
	}
	return VoteResponseValue{
		Term:    binary.LittleEndian.Uint64(value[0:8]),
		Granted: value[8] == 1,
	}, nil
}

// ---------------------------------------------------------------------------
// Topology (CmdTopology)
// Value = "nodeID@addr\nnodeID@addr\n..."
// ---------------------------------------------------------------------------

// TopologyEntry represents a single node in a topology broadcast.
type TopologyEntry struct {
	NodeID string
	Addr   string
}

// NewTopologyRequest builds a CmdTopology request from a list of entries.
func NewTopologyRequest(entries []TopologyEntry) Request {
	var sb strings.Builder
	for i, e := range entries {
		if i > 0 {
			sb.WriteString(Delimiter)
		}
		sb.WriteString(e.NodeID)
		sb.WriteString("@")
		sb.WriteString(e.Addr)
	}
	return Request{
		Cmd:   CmdTopology,
		Value: []byte(sb.String()),
	}
}

// ParseTopologyValue parses the Value field of a CmdTopology request.
func ParseTopologyValue(value []byte) []TopologyEntry {
	if len(value) == 0 {
		return nil
	}
	var entries []TopologyEntry
	for _, entry := range strings.Split(string(value), Delimiter) {
		nodeID, addr, ok := strings.Cut(entry, "@")
		if !ok || nodeID == "" || addr == "" {
			continue
		}
		entries = append(entries, TopologyEntry{NodeID: nodeID, Addr: addr})
	}
	return entries
}

// ---------------------------------------------------------------------------
// Ack/Nack (CmdAck, CmdNack)
// Value = sender's nodeID
// RequestId = quorum write request ID
// ---------------------------------------------------------------------------

// AckRequest holds the parsed fields from a CmdAck or CmdNack request.
type AckRequest struct {
	NodeID    string
	RequestId string
}

// NewAckRequest builds a CmdAck or CmdNack request.
func NewAckRequest(c Cmd, requestId string, nodeId string) Request {
	return Request{
		Cmd:       c,
		Value:     []byte(nodeId),
		RequestId: requestId,
	}
}
