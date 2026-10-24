// Package workspaceprotocol defines the binary framing used by interactive
// Workspace shell streams.
package workspaceprotocol

import (
	"encoding/binary"
	"errors"
)

const (
	HeaderSize   = 9
	MaxFrame     = 64 * 1024
	MaxChunk     = 1 << 20
	frameData    = 1
	frameSize    = 2
	frameAttach  = 3
	frameSession = 4
	frameEnd     = 5
)

var ErrInvalidFrame = errors.New("invalid Workspace shell frame")

var magic = [4]byte{'H', 'A', 'K', 'S'}

type Frame struct {
	Data    []byte
	Resize  bool
	Columns uint16
	Rows    uint16
	Attach  bool
	Session string
	End     bool
}

// EncodeAttach starts a new terminal when sessionID is empty, or resumes an
// existing terminal when it contains a valid opaque session identifier.
func EncodeAttach(sessionID string) ([]byte, error) {
	if sessionID != "" && !validSessionID(sessionID) {
		return nil, ErrInvalidFrame
	}
	return encodeControl(frameAttach, sessionID)
}

// EncodeSession returns the opaque identifier for a newly created terminal.
func EncodeSession(sessionID string) ([]byte, error) {
	if !validSessionID(sessionID) {
		return nil, ErrInvalidFrame
	}
	return encodeControl(frameSession, sessionID)
}

// EncodeEnd signals that the remote PTY exited normally.
func EncodeEnd() []byte {
	frame := make([]byte, HeaderSize)
	copy(frame, magic[:])
	frame[4] = frameEnd
	return frame
}

func encodeControl(kind byte, value string) ([]byte, error) {
	if len(value) > 64 {
		return nil, ErrInvalidFrame
	}
	frame := make([]byte, HeaderSize+len(value))
	copy(frame, magic[:])
	frame[4] = kind
	binary.BigEndian.PutUint32(frame[5:HeaderSize], uint32(len(value)))
	copy(frame[HeaderSize:], value)
	return frame, nil
}

func EncodeData(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data) > MaxFrame-HeaderSize {
		return nil, ErrInvalidFrame
	}
	frame := make([]byte, HeaderSize+len(data))
	copy(frame, magic[:])
	frame[4] = frameData
	binary.BigEndian.PutUint32(frame[5:HeaderSize], uint32(len(data)))
	copy(frame[HeaderSize:], data)
	return frame, nil
}

func EncodeResize(columns, rows uint16) ([]byte, error) {
	if columns == 0 || columns > 500 || rows == 0 || rows > 500 {
		return nil, ErrInvalidFrame
	}
	frame := make([]byte, HeaderSize+4)
	copy(frame, magic[:])
	frame[4] = frameSize
	binary.BigEndian.PutUint32(frame[5:HeaderSize], 4)
	binary.BigEndian.PutUint16(frame[HeaderSize:HeaderSize+2], columns)
	binary.BigEndian.PutUint16(frame[HeaderSize+2:], rows)
	return frame, nil
}

func Decode(frame []byte) (Frame, error) {
	if len(frame) < HeaderSize || len(frame) > MaxFrame || string(frame[:4]) != string(magic[:]) ||
		int(binary.BigEndian.Uint32(frame[5:HeaderSize])) != len(frame)-HeaderSize {
		return Frame{}, ErrInvalidFrame
	}
	switch frame[4] {
	case frameData:
		if len(frame) == HeaderSize {
			return Frame{}, ErrInvalidFrame
		}
		return Frame{Data: append([]byte(nil), frame[HeaderSize:]...)}, nil
	case frameSize:
		if len(frame) != HeaderSize+4 {
			return Frame{}, ErrInvalidFrame
		}
		columns := binary.BigEndian.Uint16(frame[HeaderSize : HeaderSize+2])
		rows := binary.BigEndian.Uint16(frame[HeaderSize+2:])
		if columns == 0 || columns > 500 || rows == 0 || rows > 500 {
			return Frame{}, ErrInvalidFrame
		}
		return Frame{Resize: true, Columns: columns, Rows: rows}, nil
	case frameAttach:
		value := string(frame[HeaderSize:])
		if value != "" && !validSessionID(value) {
			return Frame{}, ErrInvalidFrame
		}
		return Frame{Attach: true, Session: value}, nil
	case frameSession:
		value := string(frame[HeaderSize:])
		if !validSessionID(value) {
			return Frame{}, ErrInvalidFrame
		}
		return Frame{Session: value}, nil
	case frameEnd:
		if len(frame) != HeaderSize {
			return Frame{}, ErrInvalidFrame
		}
		return Frame{End: true}, nil
	default:
		return Frame{}, ErrInvalidFrame
	}
}

func validSessionID(value string) bool {
	if len(value) != 48 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

// Decoder reconstructs frames from a byte stream, where reads may split or
// coalesce writes. It rejects invalid headers and bounds buffered data.
type Decoder struct {
	buffer []byte
}

func (d *Decoder) Feed(chunk []byte) ([]Frame, error) {
	if len(chunk) == 0 {
		return nil, nil
	}
	if len(chunk) > MaxChunk {
		return nil, ErrInvalidFrame
	}
	var frames []Frame
	for len(chunk) > 0 {
		room := MaxFrame - len(d.buffer)
		size := len(chunk)
		if size > room {
			size = room
		}
		d.buffer = append(d.buffer, chunk[:size]...)
		chunk = chunk[size:]
		for len(d.buffer) >= HeaderSize {
			if string(d.buffer[:4]) != string(magic[:]) {
				return nil, ErrInvalidFrame
			}
			length := binary.BigEndian.Uint32(d.buffer[5:HeaderSize])
			kind := d.buffer[4]
			if (length == 0 && kind != frameAttach && kind != frameEnd) || length > MaxFrame-HeaderSize || uint64(HeaderSize)+uint64(length) > uint64(MaxFrame) {
				return nil, ErrInvalidFrame
			}
			frameSize := HeaderSize + int(length)
			if len(d.buffer) < frameSize {
				break
			}
			frame, err := Decode(d.buffer[:frameSize])
			if err != nil {
				return nil, err
			}
			frames = append(frames, frame)
			d.buffer = d.buffer[frameSize:]
		}
		if len(d.buffer) == MaxFrame && len(chunk) > 0 {
			return nil, ErrInvalidFrame
		}
	}
	return frames, nil
}

func (d *Decoder) Pending() int {
	if d == nil {
		return 0
	}
	return len(d.buffer)
}
