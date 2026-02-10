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
	// [cmd u8][flags u8][seq u64][waitForSeq u64][klen u32][vlen u32][key][value]
	requestHeaderSize = u8Size + u8Size + u64Size + u64Size + u32Size + u32Size // 26 bytes
	requestCmdOff     = 0
	requestFlagsOff   = requestCmdOff + u8Size      // 1
	requestSeqOff     = requestFlagsOff + u8Size    // 2
	requestWaitSeqOff = requestSeqOff + u64Size     // 10
	requestKLenOff    = requestWaitSeqOff + u64Size // 18
	requestVLenOff    = requestKLenOff + u32Size    // 22
	requestKeyOff     = requestHeaderSize           // 26

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
)

type Cmd uint8

const (
	CmdGet       Cmd = 1 // Retrieve value by key
	CmdPut       Cmd = 2 // Store key-value pair
	CmdReplicate Cmd = 3 // Establish replication connection
	CmdPing      Cmd = 4 // Health check
	CmdPromote   Cmd = 5 // Promote replica to primary
	CmdReplicaOf Cmd = 6 // Configure as replica of primary
	CmdCleanup   Cmd = 7 // Trigger value file compaction
)

type Status uint8

const (
	StatusOK              Status = 0 // Success
	StatusNotFound        Status = 1 // Key not found (GET)
	StatusError           Status = 2 // Generic server error
	StatusReadOnly        Status = 3 // Replica cannot accept writes; Value contains primary address
	StatusPong            Status = 4 // Response to PING
	StatusFullResync      Status = 5 // Primary requires full resync; Value contains snapshot
	StatusCleaning        Status = 6 // Cleanup in progress, writes temporarily rejected
	StatusReplicaTooStale Status = 7 // Replica exceeds staleness bounds; client should retry another replica or primary
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
	Value      []byte
	Seq        uint64 // Sequence number (meaningful if FlagHasSeq set: CmdPut, CmdReplicate)
	WaitForSeq uint64 // Wait for replication (meaningful if FlagHasWaitForSeq set: CmdGet strong read; 0 = eventual)
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

// EncodeRequest encodes a Request into a payload (without the outer frameLen).
//
// Uniform request format (all commands):
//
//	[cmd uint8][flags uint8][seq uint64 LE][waitForSeq uint64 LE][klen uint32 LE][vlen uint32 LE][key bytes][value bytes]
//
// Flags indicate which fields are meaningful:
//   - FlagHasSeq: seq field is used (CmdPut, CmdReplicate)
//   - FlagHasWaitForSeq: waitForSeq field is used (CmdGet with strong consistency)
func EncodeRequest(req Request) ([]byte, error) {
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
	case CmdPut, CmdReplicate:
		flags |= FlagHasSeq
	case CmdGet:
		if req.WaitForSeq > 0 {
			flags |= FlagHasWaitForSeq
		}
	}

	// Uniform header for all commands
	buf := make([]byte, requestHeaderSize+klen+vlen)
	buf[requestCmdOff] = byte(req.Cmd)
	buf[requestFlagsOff] = flags
	putU64LE(buf, requestSeqOff, req.Seq)
	putU64LE(buf, requestWaitSeqOff, req.WaitForSeq)
	putU32LE(buf, requestKLenOff, klen)
	putU32LE(buf, requestVLenOff, vlen)
	copy(buf[requestKeyOff:], req.Key)
	copy(buf[requestKeyOff+klen:], req.Value)

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
	kU32 := binary.LittleEndian.Uint32(payload[requestKLenOff : requestKLenOff+u32Size])
	vU32 := binary.LittleEndian.Uint32(payload[requestVLenOff : requestVLenOff+u32Size])

	klen, ok := lenFromU32(kU32)
	if !ok {
		return Request{}, ErrInvalidMessage
	}
	vlen, ok := lenFromU32(vU32)
	if !ok {
		return Request{}, ErrInvalidMessage
	}

	need := requestHeaderSize + klen + vlen
	if len(payload) != need {
		return Request{}, ErrInvalidMessage
	}

	key := append([]byte(nil), payload[requestKeyOff:requestKeyOff+klen]...)
	val := append([]byte(nil), payload[requestKeyOff+klen:need]...)

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

	switch resp.Status {
	case StatusOK, StatusNotFound, StatusError, StatusReadOnly, StatusPong, StatusFullResync, StatusCleaning, StatusReplicaTooStale:
		// ok
	default:
		return nil, ErrUnknownStatus
	}

	// Response payload carries value only for specific status codes:
	// - StatusOK: GET responses contain the retrieved value
	// - StatusReadOnly: Replica rejection contains primary address for redirect
	// - StatusReplicaTooStale: Stale replica rejection contains primary address
	// - StatusFullResync: Full resync responses contain database snapshot
	if resp.Status != StatusOK && resp.Status != StatusFullResync && resp.Status != StatusReadOnly && resp.Status != StatusReplicaTooStale && vlen != 0 {
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

	switch st {
	case StatusOK, StatusNotFound, StatusError, StatusReadOnly, StatusPong, StatusFullResync, StatusCleaning, StatusReplicaTooStale:
		// ok
	default:
		return Response{}, ErrUnknownStatus
	}

	// Response payload carries value only for specific status codes:
	// - StatusOK: GET responses contain the retrieved value
	// - StatusReadOnly: Replica rejection contains primary address for redirect
	// - StatusReplicaTooStale: Stale replica rejection contains primary address
	// - StatusFullResync: Full resync responses contain database snapshot
	if st != StatusOK && st != StatusFullResync && st != StatusReadOnly && st != StatusReplicaTooStale && vlen != 0 {
		return Response{}, ErrInvalidMessage
	}

	val := append([]byte(nil), payload[responseValueOff:need]...)
	return Response{Status: st, Seq: seq, Value: val}, nil
}
