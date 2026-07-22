//go:build linux

package localhooktransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
)

type Client struct {
	config Config
}

func NewClient(config Config) (*Client, error) {
	if err := config.Validate(true); err != nil {
		return nil, err
	}
	if err := validateSocketPathSyntax(config.SocketPath); err != nil {
		return nil, err
	}
	return &Client{config: config}, nil
}

func (client *Client) Send(ctx context.Context, request Request) (Response, error) {
	data, err := EncodeRequestJSON(canonicalRequest(request))
	if err != nil {
		return Response{}, err
	}
	if uint64(len(data)) > uint64(client.config.MaximumRequestBytes) {
		return Response{}, ErrRequestTooLarge
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", client.config.SocketPath)
	if err != nil {
		return Response{}, err
	}
	defer connection.Close()
	_ = connection.SetWriteDeadline(client.config.Clock.Now().Add(client.config.WriteDeadline))
	if err := writeFrame(connection, data, client.config.MaximumRequestBytes); err != nil {
		return Response{}, err
	}
	_ = connection.SetReadDeadline(client.config.Clock.Now().Add(client.config.ReadDeadline))
	responseData, err := readFrame(connection, client.config.MaximumResponseBytes)
	if errors.Is(err, ErrRequestTooLarge) {
		return Response{}, ErrResponseTooLarge
	}
	if err != nil {
		return Response{}, err
	}
	var response Response
	decoder := json.NewDecoder(bytes.NewReader(responseData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return Response{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Response{}, errors.New("response contains trailing data")
	}
	return response, nil
}
