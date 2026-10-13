package tiktokapp

import (
	"encoding/binary"
	"errors"
)

type protoField struct {
	number  int
	wire    uint8
	integer uint64
	bytes   []byte
}

func pi(number int, value uint64) protoField { return protoField{number: number, integer: value} }
func pb(number int, value []byte) protoField {
	return protoField{number: number, wire: 2, bytes: value}
}
func ps(number int, value string) protoField { return pb(number, []byte(value)) }
func encodeProto(fields ...protoField) []byte {
	out := make([]byte, 0, 512)
	for _, f := range fields {
		out = binary.AppendUvarint(out, uint64(f.number<<3)|uint64(f.wire))
		if f.wire == 0 {
			out = binary.AppendUvarint(out, f.integer)
		} else {
			out = binary.AppendUvarint(out, uint64(len(f.bytes)))
			out = append(out, f.bytes...)
		}
	}
	return out
}
func decodeProto(in []byte) ([]protoField, error) {
	var out []protoField
	for len(in) > 0 {
		tag, n := binary.Uvarint(in)
		if n <= 0 {
			return nil, errors.New("truncated or overflowing protobuf tag")
		}
		in = in[n:]
		number, wire := int(tag>>3), uint8(tag&7)
		if number < 1 || number >= 1<<29 {
			return nil, errors.New("invalid protobuf field number")
		}
		value, n := binary.Uvarint(in)
		if n <= 0 {
			return nil, errors.New("truncated or overflowing protobuf integer")
		}
		in = in[n:]
		f := protoField{number: number, wire: wire}
		switch wire {
		case 0:
			f.integer = value
		case 2:
			if value > uint64(len(in)) {
				return nil, errors.New("truncated protobuf byte field")
			}
			f.bytes = in[:int(value)]
			in = in[int(value):]
		default:
			return nil, errors.New("unsupported protobuf wire type")
		}
		out = append(out, f)
	}
	return out, nil
}
func protoBytes(fields []protoField, number int) ([]byte, error) {
	for _, f := range fields {
		if f.number == number && f.wire == 2 {
			return f.bytes, nil
		}
	}
	return nil, errors.New("missing protobuf bytes field")
}
