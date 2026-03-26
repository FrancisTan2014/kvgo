package protocol

import (
	"encoding/binary"
	"errors"
	"kvgo/transport"
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
	// [cmd u8][flags u8][seq u64][ridLen u32][klen u32][vlen u32][key][value]
	// ridLen is reserved (always 0 post-purge) for wire compatibility.
	requestHeaderSize = u8Size + u8Size + u64Size + u32Size + u32Size + u32Size // 22 bytes
	requestCmdOff     = 0
	requestFlagsOff   = requestCmdOff + u8Size     // 1
	requestSeqOff     = requestFlagsOff + u8Size   // 2
	requestRidLenOff  = requestSeqOff + u64Size    // 10
	requestKLenOff    = requestRidLenOff + u32Size // 14
	requestVLenOff    = requestKLenOff + u32Size   // 18
	requestDataOff    = requestHeaderSize          // 22

	// Response payload: [status u8][seq u64][vlen u32][value]
	responseHeaderSize = u8Size + u64Size + u32Size // 13 bytes
	responseStatusOff  = 0
	responseSeqOff     = responseStatusOff + u8Size // 1
	responseVLenOff    = responseSeqOff + u64Size   // 9
	responseValueOff   = responseHeaderSize         // 13
)

// Request flags indicate which optional fields are meaningful
const (
	FlagHasSeq = 1 << 0 // seq field is meaningful (CmdPut)
)

type Cmd uint8

const (
	CmdGet Cmd = 1 // Retrieve value by key
	CmdPut Cmd = 2 // Store key-value pair
)

type Status uint8

const (
	StatusOK       Status = iota // Success
	StatusNotFound               // Key not found (GET)
	StatusError                  // Generic server error

	statusMaxKnown // Sentinel: update when adding new status codes
)

type Request struct {
	Cmd   Cmd
	Key   []byte // CmdGet, CmdPut: the database key
	Value []byte // CmdPut: the database value
	Seq   uint64 // Sequence number (meaningful if FlagHasSeq set)
}

type Response struct {
	Status Status
	Value  []byte // StatusOK (GET): retrieved DB value
	Seq    uint64
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
		return transport.ErrFrameTooLarge
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
	return st == StatusOK
}

// EncodeRequest encodes a Request into a payload (without the outer frameLen).
//
// Uniform request format (all commands):
//
//	[cmd uint8][flags uint8][seq uint64 LE][klen uint32 LE][vlen uint32 LE][key bytes][value bytes]
//
// Flags indicate which fields are meaningful:
//   - FlagHasSeq: seq field is used (CmdPut)
func EncodeRequest(req Request) ([]byte, error) {
	klen := len(req.Key)
	if err := ensureU32Len(klen); err != nil {
		return nil, err
	}
	vlen := len(req.Value)
	if err := ensureU32Len(vlen); err != nil {
		return nil, err
	}

	var flags uint8
	if req.Cmd == CmdPut {
		flags |= FlagHasSeq
	}

	buf := make([]byte, requestHeaderSize+klen+vlen)
	buf[requestCmdOff] = byte(req.Cmd)
	buf[requestFlagsOff] = flags
	putU64LE(buf, requestSeqOff, req.Seq)
	putU32LE(buf, requestRidLenOff, 0)
	putU32LE(buf, requestKLenOff, klen)
	putU32LE(buf, requestVLenOff, vlen)
	off := requestDataOff
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

	// Parse variable-length sections in order: key, value
	off := requestDataOff + ridlen
	key := append([]byte(nil), payload[off:off+klen]...)
	off += klen
	val := append([]byte(nil), payload[off:off+vlen]...)

	req := Request{
		Cmd:   cmd,
		Key:   key,
		Value: val,
	}

	if flags&FlagHasSeq != 0 {
		req.Seq = seq
	}

	return req, nil
}

// EncodeResponse encodes a Response into a payload (without the outer frameLen).
//
// Uniform response format:
//
//	[status uint8][seq uint64 LE][vlen uint32 LE][value bytes]
//
// The value field is optional and used only for:
//   - StatusOK: Retrieved value from GET operations
func EncodeResponse(resp Response) ([]byte, error) {
	vlen := len(resp.Value)
	if err := ensureU32Len(vlen); err != nil {
		return nil, err
	}

	if resp.Status > statusMaxKnown {
		return nil, ErrUnknownStatus
	}

	// Response payload carries value only for StatusOK (GET responses).
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

	// Response payload carries value only for StatusOK (GET responses).
	if !statusCanCarryValue(st) && vlen != 0 {
		return Response{}, ErrInvalidMessage
	}

	val := append([]byte(nil), payload[responseValueOff:need]...)
	return Response{Status: st, Seq: seq, Value: val}, nil
}
