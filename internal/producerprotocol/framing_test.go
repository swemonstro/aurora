package producerprotocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// timeoutNetError is a deterministic net.Error with Timeout() == true so
// framing tests never depend on wall-clock deadlines or sleeps.
type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

// chunkThenTimeout yields its remaining chunk bytes (possibly across several
// Read calls), then every subsequent Read returns a timeout. Combined with
// io.ReadFull this models "N bytes arrived, then the deadline fired before
// the rest of the frame" without wall-clock sleeps.
type chunkThenTimeout struct {
	chunk []byte
}

func (r *chunkThenTimeout) Read(p []byte) (int, error) {
	if len(r.chunk) == 0 {
		return 0, timeoutNetError{}
	}
	n := copy(p, r.chunk)
	r.chunk = r.chunk[n:]
	return n, nil
}

func TestWriteReadFrameRoundTrip(t *testing.T) {
	var buffer bytes.Buffer
	payload := []byte(`{"hello":"world"}`)
	if err := writeFrame(&buffer, payload, 4096); err != nil {
		t.Fatal(err)
	}
	got, err := readFrame(&buffer, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

func TestReadFrameRejectsOversizedFrame(t *testing.T) {
	var buffer bytes.Buffer
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 100)
	buffer.Write(header[:])
	buffer.Write(make([]byte, 100))
	if _, err := readFrame(&buffer, 10); ErrorCodeOf(err) != CodeMessageTooLarge {
		t.Fatalf("code = %v, want message_too_large (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestWriteFrameRejectsOversizedPayload(t *testing.T) {
	var buffer bytes.Buffer
	if err := writeFrame(&buffer, make([]byte, 100), 10); ErrorCodeOf(err) != CodeMessageTooLarge {
		t.Fatalf("code = %v, want message_too_large (err=%v)", ErrorCodeOf(err), err)
	}
	if buffer.Len() != 0 {
		t.Fatalf("oversized write must not partially write: %d bytes written", buffer.Len())
	}
}

func TestReadFrameRejectsZeroLengthFrame(t *testing.T) {
	var buffer bytes.Buffer
	var header [4]byte
	buffer.Write(header[:])
	if _, err := readFrame(&buffer, 4096); ErrorCodeOf(err) != CodeMalformedMessage {
		t.Fatalf("code = %v, want malformed_message (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestReadFrameRejectsTruncatedHeader(t *testing.T) {
	buffer := bytes.NewBuffer([]byte{0, 0})
	_, err := readFrame(buffer, 4096)
	if !errors.Is(err, ErrPeerDisconnected) {
		t.Fatalf("truncated header error = %v, want ErrPeerDisconnected", err)
	}
}

func TestReadFrameRejectsTruncatedBody(t *testing.T) {
	var buffer bytes.Buffer
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 10)
	buffer.Write(header[:])
	buffer.Write([]byte("short"))
	_, err := readFrame(&buffer, 4096)
	if !errors.Is(err, ErrPeerDisconnected) {
		t.Fatalf("truncated body error = %v, want ErrPeerDisconnected", err)
	}
}

func TestMultipleFramesInSequence(t *testing.T) {
	var buffer bytes.Buffer
	frames := [][]byte{[]byte("first"), []byte("second"), []byte("third")}
	for _, frame := range frames {
		if err := writeFrame(&buffer, frame, 4096); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range frames {
		got, err := readFrame(&buffer, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	if _, err := readFrame(&buffer, 4096); !errors.Is(err, ErrPeerDisconnected) && err != io.EOF {
		t.Fatalf("expected EOF/disconnected after last frame, got %v", err)
	}
}

func TestReadFrameZeroByteTimeoutIsIdle(t *testing.T) {
	_, err := readFrame(&chunkThenTimeout{}, 4096)
	if !errors.Is(err, ErrReadTimeout) {
		t.Fatalf("error = %v, want ErrReadTimeout", err)
	}
	if ErrorCodeOf(err) != CodeReadTimeout {
		t.Fatalf("code = %v, want %v", ErrorCodeOf(err), CodeReadTimeout)
	}
	if !IsIdleReadTimeout(err) {
		t.Fatal("zero-byte read timeout must be idle/recoverable")
	}
	if errors.Is(err, ErrIncompleteFrame) {
		t.Fatal("zero-byte timeout must not be IncompleteFrame")
	}
}

func TestReadFramePartialHeaderTimeoutIsFatal(t *testing.T) {
	// Two of four length-prefix bytes, then deadline.
	_, err := readFrame(&chunkThenTimeout{chunk: []byte{0x00, 0x00}}, 4096)
	if !errors.Is(err, ErrIncompleteFrame) {
		t.Fatalf("error = %v, want ErrIncompleteFrame", err)
	}
	if ErrorCodeOf(err) != CodeIncompleteFrame {
		t.Fatalf("code = %v, want %v", ErrorCodeOf(err), CodeIncompleteFrame)
	}
	if IsIdleReadTimeout(err) {
		t.Fatal("partial header timeout must not be treated as idle")
	}
	// Still timeout-rooted for Unwrap/errors.Is, without being CodeReadTimeout.
	if !errors.Is(err, ErrReadTimeout) {
		t.Fatalf("incomplete frame should still unwrap to ErrReadTimeout, got %v", err)
	}
}

func TestReadFramePartialBodyTimeoutIsFatal(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 10)
	chunk := append(header[:], []byte("ab")...)
	_, err := readFrame(&chunkThenTimeout{chunk: chunk}, 4096)
	if !errors.Is(err, ErrIncompleteFrame) {
		t.Fatalf("error = %v, want ErrIncompleteFrame", err)
	}
	if ErrorCodeOf(err) != CodeIncompleteFrame {
		t.Fatalf("code = %v, want %v", ErrorCodeOf(err), CodeIncompleteFrame)
	}
	if IsIdleReadTimeout(err) {
		t.Fatal("partial body timeout must not be treated as idle")
	}
}

func TestReadFrameTimeoutAfterHeaderBeforeBodyIsFatal(t *testing.T) {
	// Complete 4-byte header claiming a 10-byte body, then immediate timeout
	// with zero body bytes. The header is already consumed, so retrying would
	// desynchronize framing.
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 10)
	_, err := readFrame(&chunkThenTimeout{chunk: header[:]}, 4096)
	if !errors.Is(err, ErrIncompleteFrame) {
		t.Fatalf("error = %v, want ErrIncompleteFrame", err)
	}
	if ErrorCodeOf(err) != CodeIncompleteFrame {
		t.Fatalf("code = %v, want %v", ErrorCodeOf(err), CodeIncompleteFrame)
	}
}

// TestReadFramePartialHeaderDesyncClassIsMessageTooLarge documents the
// production failure mode this fix prevents: if a caller incorrectly
// continued after consuming only part of a length prefix, the remaining
// bytes would be re-interpreted as a fresh header and can surface as
// message_too_large (the shadow-run symptom).
func TestReadFramePartialHeaderDesyncClassIsMessageTooLarge(t *testing.T) {
	// Stream as seen after two length-prefix bytes (0x00, 0x00) were already
	// consumed: the next four bytes 0xFF 0xFF 0xFF 0xFF look like an enormous
	// frame length when framing restarts mid-stream.
	remaining := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x01, 0x02}
	_, err := readFrame(bytes.NewReader(remaining), 4096)
	if ErrorCodeOf(err) != CodeMessageTooLarge {
		t.Fatalf("desynchronized restart code = %v, want message_too_large (err=%v)", ErrorCodeOf(err), err)
	}
}
