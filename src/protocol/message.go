package protocol

import (
	"encoding/binary"
	"errors"
	"math"
)

var (
	ErrInvalidMessage = errors.New("protocol: invalid message")
	ErrUnknownCmd     = errors.New("protocol: unknown cmd")
	ErrUnknownStatus  = errors.New("protocol: unknown status")
)

const (
	u8Size  = 1
	u32Size = 4
	u64Size = 8

	// Uniform request header (all commands):
	// [cmd u8][flags u8][seq u64][waitForSeq u64][ridLen u32][klen u32][vlen u32][requestId][key][value]
	requestHeaderSize = u8Size + u8Size + u64Size + u64Size + u32Size + u32Size + u32Size // 30 bytes
	requestCmdOff     = 0
	requestFlagsOff   = requestCmdOff + u8Size      // 1
	requestSeqOff     = requestFlagsOff + u8Size    // 2
	requestWaitSeqOff = requestSeqOff + u64Size     // 10
	requestRidLenOff  = requestWaitSeqOff + u64Size // 18
	requestKLenOff    = requestRidLenOff + u32Size  // 22
	requestVLenOff    = requestKLenOff + u32Size    // 26
	requestDataOff    = requestHeaderSize           // 30

	// Response payload: [status u8][seq u64][vlen u32][value]
	responseHeaderSize = u8Size + u64Size + u32Size // 13 bytes
	responseStatusOff  = 0
	responseSeqOff     = responseStatusOff + u8Size // 1
	responseVLenOff    = responseSeqOff + u64Size   // 9
	responseValueOff   = responseHeaderSize         // 13
)

// Request flags indicate which optional fields are meaningful
const (
	FlagHasSeq        = 1 << 0 // seq field is meaningful (CmdPut, CmdReplicate)
	FlagHasWaitForSeq = 1 << 1 // waitForSeq field is meaningful (CmdGet with strong read)
	FlagRequireQuorum = 1 << 2 // RequireQuorum field is meaningful (CmdPut with quorum write)
	FlagHasRequestId  = 1 << 3 // requestId field is present (quorum write tracking)
)

type Cmd uint8

const (
	CmdGet       Cmd = 1  // Retrieve value by key
	CmdPut       Cmd = 2  // Store key-value pair
	CmdReplicate Cmd = 3  // Establish replication connection
	CmdPing      Cmd = 4  // Health check
	CmdPromote   Cmd = 5  // Promote replica to primary
	CmdReplicaOf Cmd = 6  // Configure as replica of primary
	CmdCleanup   Cmd = 7  // Trigger value file compaction
	CmdAck       Cmd = 8  // Replica ACKs
	CmdNack      Cmd = 9  // Replica negative ACK (write failed)
	CmdPong      Cmd = 10 // Response to PING
)

type Status uint8

const (
	StatusOK              Status = iota // Success
	StatusNotFound                      // Key not found (GET)
	StatusError                         // Generic server error
	StatusReadOnly                      // Replica cannot accept writes; Value contains primary address
	StatusPong                          // Response to PING
	StatusFullResync                    // Primary requires full resync; Value contains snapshot
	StatusCleaning                      // Cleanup in progress, writes temporarily rejected
	StatusReplicaTooStale               // Replica exceeds staleness bounds; client should retry another replica or primary

	statusMaxKnown // Sentinel: update when adding new status codes
)

type Request struct {
	Cmd Cmd
	// Key contains the request identifier/parameter.
	//
	// - CmdGet: the key to retrieve from the database
	// - CmdPut: the key to store in the database
	// - CmdReplicaOf: unused (must be empty)
	// - CmdReplicate: unused (must be empty)
	// - CmdPing, CmdPromote, CmdCleanup: unused (must be empty)
	Key []byte
	// Value contains the payload data.
	//
	// - CmdPut: the value to store in the database
	// - CmdReplicaOf: the target primary address (host:port format)
	// - CmdReplicate: optionally contains replid for replica identification
	// - Others: unused (must be empty)
	Value         []byte
	Seq           uint64 // Sequence number (meaningful if FlagHasSeq set: CmdPut, CmdReplicate)
	WaitForSeq    uint64 // Wait for replication (meaningful if FlagHasWaitForSeq set: CmdGet strong read; 0 = eventual)
	RequireQuorum bool   // Quorum write (meaningful if FlagRequireQuorum set: CmdPut quorum write)
	RequestId     string // Unique ID for request tracing (meaningful if FlagHasRequestId set: quorum writes, ACKs)
}

type Response struct {
	Status Status
	// Value contains the response payload.
	//
	// - StatusOK (for CmdGet): the retrieved value
	// - StatusReadOnly: the primary server address (host:port) for client redirect
	// - StatusFullResync: the full database snapshot for replica resync
	// - Others: unused (must be empty)
	Value []byte
	Seq   uint64 // Sequence number at time of response (for tracking replication lag)
}

func lenFromU32(u uint32) (int, bool) {
	// On 32-bit platforms, converting a uint32 greater than MaxInt wraps and can go negative.
	// Keep this safe and explicit.
	if uint64(u) > uint64(math.MaxInt) {
		return 0, false
	}
	return int(u), true
}

func ensureU32Len(n int) error {
	if uint64(n) > uint64(^uint32(0)) {
		return ErrFrameTooLarge
	}
	return nil
}

func putU32LE(buf []byte, off int, v int) {
	binary.LittleEndian.PutUint32(buf[off:off+u32Size], uint32(v))
}

func putU64LE(buf []byte, off int, v uint64) {
	binary.LittleEndian.PutUint64(buf[off:off+u64Size], v)
}

// statusCanCarryValue returns true if the status code is allowed to have a non-empty value field.
func statusCanCarryValue(st Status) bool {
	return st == StatusOK || st == StatusFullResync || st == StatusReadOnly || st == StatusReplicaTooStale
}

// EncodeRequest encodes a Request into a payload (without the outer frameLen).
//
// Uniform request format (all commands):
//
//	[cmd uint8][flags uint8][seq uint64 LE][waitForSeq uint64 LE][ridLen uint32 LE][klen uint32 LE][vlen uint32 LE][requestId bytes][key bytes][value bytes]
//
// Flags indicate which fields are meaningful:
//   - FlagHasSeq: seq field is used (CmdPut, CmdReplicate)
//   - FlagHasWaitForSeq: waitForSeq field is used (CmdGet with strong consistency)
//   - FlagHasRequestId: requestId field is present (quorum writes, ACKs)
func EncodeRequest(req Request) ([]byte, error) {
	ridlen := len(req.RequestId)
	if err := ensureU32Len(ridlen); err != nil {
		return nil, err
	}
	klen := len(req.Key)
	if err := ensureU32Len(klen); err != nil {
		return nil, err
	}
	vlen := len(req.Value)
	if err := ensureU32Len(vlen); err != nil {
		return nil, err
	}

	// Determine flags based on command and fields
	var flags uint8
	switch req.Cmd {
	case CmdPut:
		flags |= FlagHasSeq
		if req.RequireQuorum {
			flags |= FlagRequireQuorum
		}
		if req.RequestId != "" {
			flags |= FlagHasRequestId
		}
	case CmdReplicate, CmdPing:
		flags |= FlagHasSeq
	case CmdGet:
		if req.WaitForSeq > 0 {
			flags |= FlagHasWaitForSeq
		}
	case CmdAck:
		if req.RequestId != "" {
			flags |= FlagHasRequestId
		}
	}

	// Uniform header for all commands
	buf := make([]byte, requestHeaderSize+ridlen+klen+vlen)
	buf[requestCmdOff] = byte(req.Cmd)
	buf[requestFlagsOff] = flags
	putU64LE(buf, requestSeqOff, req.Seq)
	putU64LE(buf, requestWaitSeqOff, req.WaitForSeq)
	putU32LE(buf, requestRidLenOff, ridlen)
	putU32LE(buf, requestKLenOff, klen)
	putU32LE(buf, requestVLenOff, vlen)
	// Variable-length sections in order: requestId, key, value
	off := requestDataOff
	copy(buf[off:], req.RequestId)
	off += ridlen
	copy(buf[off:], req.Key)
	off += klen
	copy(buf[off:], req.Value)

	return buf, nil
}

// DecodeRequest decodes a payload (without the outer frameLen) into a Request.
// It copies key/value bytes so the returned Request does not alias the input.
func DecodeRequest(payload []byte) (Request, error) {
	if len(payload) < requestHeaderSize {
		return Request{}, ErrInvalidMessage
	}

	cmd := Cmd(payload[requestCmdOff])
	flags := payload[requestFlagsOff]
	seq := binary.LittleEndian.Uint64(payload[requestSeqOff : requestSeqOff+u64Size])
	waitForSeq := binary.LittleEndian.Uint64(payload[requestWaitSeqOff : requestWaitSeqOff+u64Size])
	ridU32 := binary.LittleEndian.Uint32(payload[requestRidLenOff : requestRidLenOff+u32Size])
	kU32 := binary.LittleEndian.Uint32(payload[requestKLenOff : requestKLenOff+u32Size])
	vU32 := binary.LittleEndian.Uint32(payload[requestVLenOff : requestVLenOff+u32Size])

	ridlen, ok := lenFromU32(ridU32)
	if !ok {
		return Request{}, ErrInvalidMessage
	}
	klen, ok := lenFromU32(kU32)
	if !ok {
		return Request{}, ErrInvalidMessage
	}
	vlen, ok := lenFromU32(vU32)
	if !ok {
		return Request{}, ErrInvalidMessage
	}

	need := requestHeaderSize + ridlen + klen + vlen
	if len(payload) != need {
		return Request{}, ErrInvalidMessage
	}

	// Parse variable-length sections in order: requestId, key, value
	off := requestDataOff
	rid := string(payload[off : off+ridlen])
	off += ridlen
	key := append([]byte(nil), payload[off:off+klen]...)
	off += klen
	val := append([]byte(nil), payload[off:off+vlen]...)

	req := Request{
		Cmd:   cmd,
		Key:   key,
		Value: val,
	}

	// Apply flags: only set fields if flag indicates they're meaningful
	if flags&FlagHasSeq != 0 {
		req.Seq = seq
	}
	if flags&FlagHasWaitForSeq != 0 {
		req.WaitForSeq = waitForSeq
	}
	if flags&FlagRequireQuorum != 0 {
		req.RequireQuorum = true
	}
	if flags&FlagHasRequestId != 0 {
		req.RequestId = rid
	}

	return req, nil
}

// EncodeResponse encodes a Response into a payload (without the outer frameLen).
//
// Uniform response format:
//
//	[status uint8][seq uint64 LE][vlen uint32 LE][value bytes]
//
// The seq field indicates the server's sequence number at time of response (for replication tracking).
// The value field is optional and used only for:
//   - StatusOK: Retrieved value from GET operations
//   - StatusReadOnly: Primary server address for redirect (host:port)
//   - StatusFullResync: Full database snapshot for replica resync
func EncodeResponse(resp Response) ([]byte, error) {
	vlen := len(resp.Value)
	if err := ensureU32Len(vlen); err != nil {
		return nil, err
	}

	if resp.Status > statusMaxKnown {
		return nil, ErrUnknownStatus
	}

	// Response payload carries value only for specific status codes:
	// - StatusOK: GET responses contain the retrieved value
	// - StatusReadOnly: Replica rejection contains primary address for redirect
	// - StatusReplicaTooStale: Stale replica rejection contains primary address
	// - StatusFullResync: Full resync responses contain database snapshot
	if !statusCanCarryValue(resp.Status) && vlen != 0 {
		return nil, ErrInvalidMessage
	}

	buf := make([]byte, responseHeaderSize+vlen)
	buf[responseStatusOff] = byte(resp.Status)
	putU64LE(buf, responseSeqOff, resp.Seq)
	putU32LE(buf, responseVLenOff, vlen)
	copy(buf[responseValueOff:], resp.Value)
	return buf, nil
}

// DecodeResponse decodes a payload (without the outer frameLen) into a Response.
// It copies value bytes so the returned Response does not alias the input.
func DecodeResponse(payload []byte) (Response, error) {
	if len(payload) < responseHeaderSize {
		return Response{}, ErrInvalidMessage
	}
	st := Status(payload[responseStatusOff])
	seq := binary.LittleEndian.Uint64(payload[responseSeqOff : responseSeqOff+u64Size])
	vU32 := binary.LittleEndian.Uint32(payload[responseVLenOff : responseVLenOff+u32Size])
	vlen, ok := lenFromU32(vU32)
	if !ok {
		return Response{}, ErrInvalidMessage
	}
	need := responseHeaderSize + vlen
	if len(payload) != need {
		return Response{}, ErrInvalidMessage
	}

	if st > statusMaxKnown {
		return Response{}, ErrUnknownStatus
	}

	// Response payload carries value only for specific status codes:
	// - StatusOK: GET responses contain the retrieved value
	// - StatusReadOnly: Replica rejection contains primary address for redirect
	// - StatusReplicaTooStale: Stale replica rejection contains primary address
	// - StatusFullResync: Full resync responses contain database snapshot
	if !statusCanCarryValue(st) && vlen != 0 {
		return Response{}, ErrInvalidMessage
	}

	val := append([]byte(nil), payload[responseValueOff:need]...)
	return Response{Status: st, Seq: seq, Value: val}, nil
}
