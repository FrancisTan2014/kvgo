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

	// Request payload: [cmd u8][klen u32][vlen u32][key][value]
	// For CmdPut: [cmd u8][klen u32][vlen u32][seq u64][key][value]
	requestHeaderSize    = u8Size + u32Size + u32Size
	requestHeaderSizePut = requestHeaderSize + u64Size // includes seq
	requestCmdOff        = 0
	requestKLenOff       = requestCmdOff + u8Size
	requestVLenOff       = requestKLenOff + u32Size
	requestSeqOff        = requestVLenOff + u32Size // only for OpPut
	requestKeyOff        = requestHeaderSize
	requestKeyOffPut     = requestHeaderSizePut // key starts after seq for OpPut

	// Response payload: [status u8][vlen u32][value]
	responseHeaderSize = u8Size + u32Size
	responseStatusOff  = 0
	responseVLenOff    = responseStatusOff + u8Size
	responseValueOff   = responseHeaderSize
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
	StatusOK         Status = 0 // Success
	StatusNotFound   Status = 1 // Key not found (GET)
	StatusError      Status = 2 // Generic server error
	StatusReadOnly   Status = 3 // Replica cannot accept writes; Value contains primary address
	StatusPong       Status = 4 // Response to PING
	StatusFullResync Status = 5 // Primary requires full resync; Value contains snapshot
	StatusCleaning   Status = 6 // Cleanup in progress, writes temporarily rejected
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
	Value []byte
	Seq   uint64 // sequence number for replication (used by CmdPut and CmdReplicate)
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
// Request payload formats by command:
//
// Standard format (CmdGet, CmdReplicaOf):
//
//	[cmd uint8][klen uint32 LE][vlen uint32 LE][key bytes][value bytes]
//
// CmdPut with sequence number for replication:
//
//	[cmd uint8][klen uint32 LE][vlen uint32 LE][seq uint64 LE][key bytes][value bytes]
//
// CmdReplicate with sequence number and optional replid:
//
//	[cmd uint8][klen=0 uint32 LE][vlen uint32 LE][seq uint64 LE][replid bytes]
//
// Command-only (CmdPing, CmdPromote, CmdCleanup):
//
//	[cmd uint8][klen=0 uint32 LE][vlen=0 uint32 LE]
func EncodeRequest(req Request) ([]byte, error) {
	klen := len(req.Key)
	if err := ensureU32Len(klen); err != nil {
		return nil, err
	}
	vlen := len(req.Value)
	if err := ensureU32Len(vlen); err != nil {
		return nil, err
	}

	switch req.Cmd {
	case CmdGet:
		if vlen != 0 {
			return nil, ErrInvalidMessage
		}
		buf := make([]byte, requestHeaderSize+klen)
		buf[requestCmdOff] = byte(req.Cmd)
		putU32LE(buf, requestKLenOff, klen)
		putU32LE(buf, requestVLenOff, 0)
		copy(buf[requestKeyOff:], req.Key)
		return buf, nil

	case CmdPut:
		buf := make([]byte, requestHeaderSizePut+klen+vlen)
		buf[requestCmdOff] = byte(req.Cmd)
		putU32LE(buf, requestKLenOff, klen)
		putU32LE(buf, requestVLenOff, vlen)
		putU64LE(buf, requestSeqOff, req.Seq)
		copy(buf[requestKeyOffPut:requestKeyOffPut+klen], req.Key)
		copy(buf[requestKeyOffPut+klen:], req.Value)
		return buf, nil

	case CmdReplicate:
		// Replicate carries seq (replica's last applied seq) and optionally replid in Value.
		if klen != 0 {
			return nil, ErrInvalidMessage
		}
		buf := make([]byte, requestHeaderSize+u64Size+vlen)
		buf[requestCmdOff] = byte(req.Cmd)
		putU32LE(buf, requestKLenOff, 0)
		putU32LE(buf, requestVLenOff, vlen)
		putU64LE(buf, requestKeyOff, req.Seq)        // seq follows header
		copy(buf[requestKeyOff+u64Size:], req.Value) // replid follows seq
		return buf, nil

	case CmdPing, CmdPromote, CmdCleanup:
		buf := make([]byte, requestHeaderSize)
		buf[requestCmdOff] = byte(req.Cmd)
		putU32LE(buf, requestKLenOff, 0)
		putU32LE(buf, requestVLenOff, 0)
		return buf, nil

	case CmdReplicaOf:
		// To make the protocol simple, we reuse the `Value` field here
		buf := make([]byte, requestHeaderSize+vlen)
		buf[requestCmdOff] = byte(req.Cmd)
		putU32LE(buf, requestKLenOff, klen)
		putU32LE(buf, requestVLenOff, vlen)
		copy(buf[requestKeyOff:], req.Key)
		copy(buf[requestHeaderSize+klen:], req.Value)
		return buf, nil

	default:
		return nil, ErrUnknownCmd
	}
}

// DecodeRequest decodes a payload (without the outer frameLen) into a Request.
// It copies key/value bytes so the returned Request does not alias the input.
func DecodeRequest(payload []byte) (Request, error) {
	if len(payload) < requestHeaderSize {
		return Request{}, ErrInvalidMessage
	}

	cmd := Cmd(payload[requestCmdOff])
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

	switch cmd {
	case CmdGet:
		if vlen != 0 {
			return Request{}, ErrInvalidMessage
		}
		need := requestHeaderSize + klen
		if len(payload) != need {
			return Request{}, ErrInvalidMessage
		}
		key := append([]byte(nil), payload[requestKeyOff:requestKeyOff+klen]...)
		return Request{Cmd: cmd, Key: key}, nil

	case CmdPut:
		need := requestHeaderSizePut + klen + vlen
		if len(payload) != need {
			return Request{}, ErrInvalidMessage
		}
		seq := binary.LittleEndian.Uint64(payload[requestSeqOff : requestSeqOff+u64Size])
		key := append([]byte(nil), payload[requestKeyOffPut:requestKeyOffPut+klen]...)
		val := append([]byte(nil), payload[requestKeyOffPut+klen:need]...)
		return Request{Cmd: cmd, Key: key, Value: val, Seq: seq}, nil

	case CmdReplicate:
		// Replicate carries seq and optionally replid in Value.
		if klen != 0 {
			return Request{}, ErrInvalidMessage
		}
		need := requestHeaderSize + u64Size + vlen
		if len(payload) != need {
			return Request{}, ErrInvalidMessage
		}
		seq := binary.LittleEndian.Uint64(payload[requestKeyOff : requestKeyOff+u64Size])
		val := append([]byte(nil), payload[requestKeyOff+u64Size:need]...)
		return Request{Cmd: cmd, Value: val, Seq: seq}, nil

	case CmdReplicaOf:
		if klen != 0 {
			return Request{}, ErrInvalidMessage
		}
		need := requestHeaderSize + vlen
		if len(payload) != need {
			return Request{}, ErrInvalidMessage
		}
		val := append([]byte(nil), payload[requestHeaderSize:need]...)
		return Request{Cmd: cmd, Value: val}, nil

	case CmdPing, CmdPromote, CmdCleanup:
		// These requests carry no data
		if klen != 0 || vlen != 0 {
			return Request{}, ErrInvalidMessage
		}
		if len(payload) != requestHeaderSize {
			return Request{}, ErrInvalidMessage
		}
		return Request{Cmd: cmd}, nil

	default:
		return Request{}, ErrUnknownCmd
	}
}

// EncodeResponse encodes a Response into a payload (without the outer frameLen).
//
// Response payload format:
//
//	[status uint8][vlen uint32 LE][value bytes]
//
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
	case StatusOK, StatusNotFound, StatusError, StatusReadOnly, StatusPong, StatusFullResync:
		// ok
	default:
		return nil, ErrUnknownStatus
	}

	// Response payload carries value only for specific status codes:
	// - StatusOK: GET responses contain the retrieved value
	// - StatusReadOnly: Replica rejection contains primary address for redirect
	// - StatusFullResync: Full resync responses contain database snapshot
	if resp.Status != StatusOK && resp.Status != StatusFullResync && resp.Status != StatusReadOnly && vlen != 0 {
		return nil, ErrInvalidMessage
	}

	buf := make([]byte, responseHeaderSize+vlen)
	buf[responseStatusOff] = byte(resp.Status)
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
	case StatusOK, StatusNotFound, StatusError, StatusReadOnly, StatusPong, StatusFullResync:
		// ok
	default:
		return Response{}, ErrUnknownStatus
	}

	// Response payload carries value only for specific status codes:
	// - StatusOK: GET responses contain the retrieved value
	// - StatusReadOnly: Replica rejection contains primary address for redirect
	// - StatusFullResync: Full resync responses contain database snapshot
	if st != StatusOK && st != StatusFullResync && st != StatusReadOnly && vlen != 0 {
		return Response{}, ErrInvalidMessage
	}

	val := append([]byte(nil), payload[responseValueOff:need]...)
	return Response{Status: st, Value: val}, nil
}
