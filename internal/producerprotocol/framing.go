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
//
// Read-deadline semantics are framing-aware:
//   - a timeout with zero bytes of this frame consumed is CodeReadTimeout
//     (idle; safe to retry on the same stream);
//   - a timeout after any header or body byte has been consumed is
//     CodeIncompleteFrame (fatal; the stream must not be reused).
func readFrame(reader io.Reader, maximum uint32) ([]byte, error) {
	var header [frameHeaderSize]byte
	n, err := io.ReadFull(reader, header[:])
	if err != nil {
		return nil, classifyFrameReadError(err, n)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return nil, protocolError(CodeMalformedMessage, errors.New("empty frame"))
	}
	if size > maximum {
		return nil, protocolError(CodeMessageTooLarge, ErrMessageTooLarge)
	}
	data := make([]byte, size)
	n, err = io.ReadFull(reader, data)
	if err != nil {
		// The 4-byte header is already fully consumed, so any failure here
		// (including a timeout before the first body byte) desynchronizes
		// the stream if the caller were to continue.
		return nil, classifyFrameReadError(err, frameHeaderSize+n)
	}
	return data, nil
}

// classifyFrameReadError maps a raw read failure from readFrame, taking into
// account how many bytes of the current frame were already consumed.
// bytesConsumed == 0 preserves the ordinary idle CodeReadTimeout path;
// bytesConsumed > 0 turns a read timeout into CodeIncompleteFrame so callers
// never retry framing on a desynchronized stream. Disconnects and other
// classifications are left unchanged (already fatal for a connection loop).
func classifyFrameReadError(err error, bytesConsumed int) error {
	classified := classifyIOError(err, true)
	if bytesConsumed <= 0 || !errors.Is(classified, ErrReadTimeout) {
		return classified
	}
	return protocolError(CodeIncompleteFrame, &wrappedError{
		sentinel: ErrIncompleteFrame,
		cause:    classified,
	})
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
