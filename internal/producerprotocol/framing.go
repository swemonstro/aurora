package producerprotocol

import (
	"encoding/binary"
	"errors"
	"io"
)

// frameHeaderSize is the width of the deterministic length prefix: a fixed
// 4-byte big-endian frame length, matching the proven framing pattern used
// by internal/localhooktransport.
const frameHeaderSize = 4

// readFrame reads one length-prefixed frame, rejecting frames of zero length
// or larger than maximum before allocating a buffer for the body.
func readFrame(reader io.Reader, maximum uint32) ([]byte, error) {
	var header [frameHeaderSize]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, classifyIOError(err, true)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return nil, protocolError(CodeMalformedMessage, errors.New("empty frame"))
	}
	if size > maximum {
		return nil, protocolError(CodeMessageTooLarge, ErrMessageTooLarge)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, classifyIOError(err, true)
	}
	return data, nil
}

// writeFrame writes one length-prefixed frame.
func writeFrame(writer io.Writer, data []byte, maximum uint32) error {
	if uint64(len(data)) > uint64(maximum) {
		return protocolError(CodeMessageTooLarge, ErrMessageTooLarge)
	}
	var header [frameHeaderSize]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if err := writeAll(writer, header[:]); err != nil {
		return classifyIOError(err, false)
	}
	if err := writeAll(writer, data); err != nil {
		return classifyIOError(err, false)
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrUnexpectedEOF
		}
		data = data[written:]
	}
	return nil
}
