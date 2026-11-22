// Package hubble reads network flows from Hubble (the Cilium observer API served by hubble-relay) and
// turns them into traffic.Flow for the traffic collector (M2-H2). It also delivers sign-in events to a
// team's log aggregator (M2-H4).
//
// The Cilium API module is large and pulls the whole agent into the build, so the few messages used
// here are encoded and decoded by hand with protowire. The field numbers are those of cilium/api/v1
// (flow.proto, observer.proto); testdata/flows.txt holds responses recorded from the generated Cilium
// types, and the decoder test reads them.
package hubble

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/traffic"
)

// Field numbers of observer.GetFlowsResponse and flow.Flow (cilium api v1).
const (
	respFlow = 1

	flowTime             = 1
	flowVerdict          = 2
	flowIP               = 5
	flowL4               = 6
	flowSource           = 8
	flowDestinationNames = 14
	flowDropReasonDesc   = 25
	flowTrafficDirection = 22
	flowIsReply          = 26

	verdictForwarded = 1
	verdictDropped   = 2

	dropPolicyDenied = 133

	directionEgress = 2

	l4TCP = 1
	l4UDP = 2

	endpointNamespace = 3
	endpointLabels    = 4
	endpointPodName   = 5

	portDestination = 2
	tcpFlags        = 3
	flagSYN         = 2
	flagACK         = 5
)

var errMalformed = errors.New("hubble: malformed flow")

// DecodeResponse decodes one GetFlowsResponse. ok is false for a response that carries no flow (node
// status and lost-event notices).
func DecodeResponse(b []byte) (f traffic.Flow, ok bool, err error) {
	var flowBytes []byte
	err = walk(b, func(num protowire.Number, typ protowire.Type, v value) error {
		if num == respFlow && typ == protowire.BytesType {
			flowBytes = v.bytes
		}
		return nil
	})
	if err != nil {
		return traffic.Flow{}, false, err
	}
	if flowBytes == nil {
		return traffic.Flow{}, false, nil
	}
	f, err = decodeFlow(flowBytes)
	return f, err == nil, err
}

func decodeFlow(b []byte) (traffic.Flow, error) {
	var (
		f       traffic.Flow
		verdict uint64
		drop    uint64
		dir     uint64
	)
	err := walk(b, func(num protowire.Number, typ protowire.Type, v value) error {
		switch {
		case num == flowTime && typ == protowire.BytesType:
			t, err := decodeTimestamp(v.bytes)
			f.Time = t
			return err
		case num == flowVerdict && typ == protowire.VarintType:
			verdict = v.varint
		case num == flowDropReasonDesc && typ == protowire.VarintType:
			drop = v.varint
		case num == flowTrafficDirection && typ == protowire.VarintType:
			dir = v.varint
		case num == flowL4 && typ == protowire.BytesType:
			return decodeL4(v.bytes, &f)
		case num == flowSource && typ == protowire.BytesType:
			return decodeEndpoint(v.bytes, &f)
		case num == flowDestinationNames && typ == protowire.BytesType:
			f.DstNames = append(f.DstNames, string(v.bytes))
		case num == flowIsReply && typ == protowire.BytesType:
			// BoolValue{value = 1}
			return walk(v.bytes, func(n protowire.Number, t protowire.Type, x value) error {
				if n == 1 && t == protowire.VarintType {
					f.Reply = x.varint != 0
				}
				return nil
			})
		}
		return nil
	})
	if err != nil {
		return traffic.Flow{}, err
	}
	f.Egress = dir == directionEgress
	f.Forwarded = verdict == verdictForwarded
	f.PolicyDenied = verdict == verdictDropped && drop == dropPolicyDenied
	return f, nil
}

func decodeTimestamp(b []byte) (time.Time, error) {
	var sec, nsec int64
	err := walk(b, func(num protowire.Number, typ protowire.Type, v value) error {
		if typ != protowire.VarintType {
			return nil
		}
		switch num {
		case 1:
			sec = int64(v.varint)
		case 2:
			nsec = int64(v.varint)
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, nsec).UTC(), nil
}

func decodeL4(b []byte, f *traffic.Flow) error {
	return walk(b, func(num protowire.Number, typ protowire.Type, v value) error {
		if typ != protowire.BytesType {
			return nil
		}
		switch num {
		case l4TCP:
			f.Proto = "tcp"
		case l4UDP:
			f.Proto = "udp"
		default:
			return nil
		}
		return walk(v.bytes, func(n protowire.Number, t protowire.Type, x value) error {
			switch {
			case n == portDestination && t == protowire.VarintType:
				f.DstPort = int(x.varint)
			case num == l4TCP && n == tcpFlags && t == protowire.BytesType:
				return walk(x.bytes, func(fn protowire.Number, ft protowire.Type, fv value) error {
					if ft != protowire.VarintType {
						return nil
					}
					switch fn {
					case flagSYN:
						f.SYN = fv.varint != 0
					case flagACK:
						f.ACK = fv.varint != 0
					}
					return nil
				})
			}
			return nil
		})
	})
}

func decodeEndpoint(b []byte, f *traffic.Flow) error {
	return walk(b, func(num protowire.Number, typ protowire.Type, v value) error {
		if typ != protowire.BytesType {
			return nil
		}
		switch num {
		case endpointNamespace:
			f.SrcNamespace = string(v.bytes)
		case endpointLabels:
			f.SrcLabels = append(f.SrcLabels, string(v.bytes))
		case endpointPodName:
			f.SrcPod = string(v.bytes)
		}
		return nil
	})
}

// value is one decoded field value: varint for VarintType, bytes for BytesType; fixed-width values are
// skipped.
type value struct {
	varint uint64
	bytes  []byte
}

// walk calls fn for every field of the message b.
func walk(b []byte, fn func(protowire.Number, protowire.Type, value) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return fmt.Errorf("%w: %v", errMalformed, protowire.ParseError(n))
		}
		b = b[n:]
		var v value
		switch typ {
		case protowire.VarintType:
			x, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return fmt.Errorf("%w: %v", errMalformed, protowire.ParseError(m))
			}
			v.varint, n = x, m
		case protowire.BytesType:
			x, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return fmt.Errorf("%w: %v", errMalformed, protowire.ParseError(m))
			}
			v.bytes, n = x, m
		default:
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return fmt.Errorf("%w: %v", errMalformed, protowire.ParseError(m))
			}
			n = m
		}
		if err := fn(num, typ, v); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}
