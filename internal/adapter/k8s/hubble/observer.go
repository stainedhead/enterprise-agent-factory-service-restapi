package hubble

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/traffic"
)

// getFlowsMethod is observer.Observer/GetFlows.
const getFlowsMethod = "/observer.Observer/GetFlows"

// Field numbers of observer.GetFlowsRequest and flow.FlowFilter.
const (
	reqFollow    = 3
	reqWhitelist = 6

	filterTrafficDirection = 30
)

// Config configures an Observer.
type Config struct {
	// Address is the host:port of hubble-relay.
	Address string
	// TLS secures the connection; nil dials in plain text (hubble-relay's in-cluster default).
	TLS *tls.Config
	// DialOptions are added to the connection's options (tests).
	DialOptions []grpc.DialOption
}

// Observer implements traffic.FlowSource on the Hubble observer API.
type Observer struct{ cfg Config }

// NewObserver validates cfg.
func NewObserver(cfg Config) (*Observer, error) {
	if cfg.Address == "" {
		return nil, errors.New("hubble: an address is required")
	}
	return &Observer{cfg: cfg}, nil
}

var _ traffic.FlowSource = (*Observer)(nil)

// rawCodec passes message bytes through untouched: the messages are encoded and decoded by hand.
type rawCodec struct{}

func (rawCodec) Marshal(v any) (mem.BufferSlice, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("hubble: cannot marshal %T", v)
	}
	return mem.BufferSlice{mem.SliceBuffer(*b)}, nil
}

func (rawCodec) Unmarshal(data mem.BufferSlice, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("hubble: cannot unmarshal into %T", v)
	}
	*b = data.Materialize()
	return nil
}

func (rawCodec) Name() string { return "proto" }

// getFlowsRequest encodes GetFlowsRequest{follow: true, whitelist: [{traffic_direction: [EGRESS]}]}:
// new flows only, leaving the source pod.
func getFlowsRequest() []byte {
	var filter []byte
	filter = protowire.AppendTag(filter, filterTrafficDirection, protowire.BytesType)
	filter = protowire.AppendBytes(filter, protowire.AppendVarint(nil, directionEgress)) // packed enum
	var req []byte
	req = protowire.AppendTag(req, reqFollow, protowire.VarintType)
	req = protowire.AppendVarint(req, 1)
	req = protowire.AppendTag(req, reqWhitelist, protowire.BytesType)
	req = protowire.AppendBytes(req, filter)
	return req
}

// Stream implements traffic.FlowSource: it follows the flows until ctx ends or the stream breaks.
func (o *Observer) Stream(ctx context.Context, handle func(traffic.Flow)) error {
	creds := credentials.TransportCredentials(insecure.NewCredentials())
	if o.cfg.TLS != nil {
		creds = credentials.NewTLS(o.cfg.TLS)
	}
	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(creds), grpc.WithDefaultCallOptions(grpc.ForceCodecV2(rawCodec{}))}, o.cfg.DialOptions...)
	conn, err := grpc.NewClient(o.cfg.Address, opts...)
	if err != nil {
		return fmt.Errorf("hubble: dial %s: %w", o.cfg.Address, err)
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, getFlowsMethod)
	if err != nil {
		return fmt.Errorf("hubble: open GetFlows: %w", err)
	}
	req := getFlowsRequest()
	if err := stream.SendMsg(&req); err != nil {
		return fmt.Errorf("hubble: send request: %w", err)
	}
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("hubble: close send: %w", err)
	}
	for {
		var msg []byte
		if err := stream.RecvMsg(&msg); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("hubble: receive: %w", err)
		}
		f, ok, err := DecodeResponse(msg)
		if err != nil {
			// A message that cannot be decoded is skipped; the stream goes on.
			continue
		}
		if ok {
			handle(f)
		}
	}
}
