package producerprotocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

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
