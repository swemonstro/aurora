package localhooktransport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
)

type ProtocolError struct {
	Code ErrorCode
	Err  error
}

func (err *ProtocolError) Error() string {
	if err == nil {
		return "protocol error"
	}
	return string(err.Code)
}

func (err *ProtocolError) Unwrap() error { return err.Err }

func protocolError(code ErrorCode, err error) error {
	return &ProtocolError{Code: code, Err: err}
}

func errorCode(err error) ErrorCode {
	var protocol *ProtocolError
	if errors.As(err, &protocol) {
		return protocol.Code
	}
	switch {
	case errors.Is(err, ErrUnauthorizedPeer):
		return CodeUnauthorizedPeer
	case errors.Is(err, ErrRequestTooLarge):
		return CodeRequestTooLarge
	case errors.Is(err, ErrResponseTooLarge):
		return CodeResponseTooLarge
	case errors.Is(err, ErrPeerDisconnected), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return CodePeerDisconnected
	case errors.Is(err, ErrReadTimeout):
		return CodeReadTimeout
	case errors.Is(err, ErrWriteTimeout):
		return CodeWriteTimeout
	case errors.Is(err, ErrReplayConflict):
		return CodeRequestIDConflict
	case errors.Is(err, ErrReplayCacheFull):
		return CodeReplayCacheFull
	default:
		return CodeInternalError
	}
}

func DecodeRequestJSON(data []byte) (Request, error) {
	if len(data) == 0 {
		return Request{}, protocolError(CodeMalformedRequest, errors.New("empty request"))
	}
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		code := CodeMalformedRequest
		if strings.Contains(err.Error(), "unknown field") {
			code = CodeUnknownField
		}
		return Request{}, protocolError(code, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return Request{}, protocolError(CodeMalformedRequest, err)
	}
	return request, nil
}

func EncodeRequestJSON(request Request) ([]byte, error) {
	return json.Marshal(request)
}

func EncodeResponseJSON(response Response, maximum uint32) ([]byte, error) {
	data, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) > uint64(maximum) {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

func readFrame(reader io.Reader, maximum uint32) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, classifyIOError(err, true)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return nil, protocolError(CodeMalformedRequest, errors.New("empty frame"))
	}
	if size > maximum {
		return nil, ErrRequestTooLarge
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, classifyIOError(err, true)
	}
	return data, nil
}

func writeFrame(writer io.Writer, data []byte, maximum uint32) error {
	if uint64(len(data)) > uint64(maximum) {
		return ErrResponseTooLarge
	}
	var header [4]byte
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

func classifyIOError(err error, read bool) error {
	if err == nil {
		return nil
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		if read {
			return fmt.Errorf("%w: %v", ErrReadTimeout, err)
		}
		return fmt.Errorf("%w: %v", ErrWriteTimeout, err)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %v", ErrPeerDisconnected, err)
	}
	return err
}
