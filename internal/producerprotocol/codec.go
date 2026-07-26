package producerprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// DecodeMessageJSON decodes exactly one JSON object into a Message. Unknown
// fields and trailing JSON values are rejected; a wire schema change is
// always a protocol_version change, never a silent extension.
func DecodeMessageJSON(data []byte, maximum uint32) (Message, error) {
	if len(data) == 0 {
		return Message{}, protocolError(CodeMalformedMessage, errors.New("empty message"))
	}
	if uint64(len(data)) > uint64(maximum) {
		return Message{}, protocolError(CodeMessageTooLarge, ErrMessageTooLarge)
	}
	var msg Message
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&msg); err != nil {
		code := CodeMalformedMessage
		if strings.Contains(err.Error(), "unknown field") {
			code = CodeUnknownField
		}
		return Message{}, protocolError(code, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return Message{}, protocolError(CodeMalformedMessage, err)
	}
	return msg, nil
}

// EncodeMessageJSON encodes msg, rejecting output that would exceed maximum.
func EncodeMessageJSON(msg Message, maximum uint32) ([]byte, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) > uint64(maximum) {
		return nil, ErrMessageTooLarge
	}
	return data, nil
}
