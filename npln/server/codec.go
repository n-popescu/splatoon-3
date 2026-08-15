package server

import (
	"google.golang.org/grpc/encoding"
	_ "google.golang.org/grpc/encoding/proto" // registers the default proto codec we delegate to
	"google.golang.org/grpc/mem"
)

// RawMessage is a message body captured verbatim.
//
// It exists for one reason: when the console calls a method this server does not
// implement, we want the actual bytes it sent, not just the method name. gRPC
// only hands a handler decoded messages, so the codec below recognises this type
// and copies the wire bytes into it instead of decoding them.
//
// Everything else keeps going through the normal protobuf codec, so this cannot
// affect a real service.
type RawMessage struct {
	Data []byte
}

// rawAwareCodec is the "proto" codec with RawMessage support bolted on.
type rawAwareCodec struct {
	base encoding.CodecV2
}

// Name must stay "proto": it is the content subtype the client asks for
// (application/grpc+proto), so this codec replaces the default one for it.
func (c rawAwareCodec) Name() string { return "proto" }

func (c rawAwareCodec) Marshal(v any) (mem.BufferSlice, error) {
	if raw, ok := v.(*RawMessage); ok {
		return mem.BufferSlice{mem.SliceBuffer(raw.Data)}, nil
	}
	return c.base.Marshal(v)
}

func (c rawAwareCodec) Unmarshal(data mem.BufferSlice, v any) error {
	if raw, ok := v.(*RawMessage); ok {
		raw.Data = data.Materialize()
		return nil
	}
	return c.base.Unmarshal(data, v)
}

// installRawCodec wraps the registered proto codec once, at start-up. Safe to
// call twice (the second call would simply wrap our own wrapper, which still
// delegates correctly), but main calls it exactly once.
func installRawCodec() {
	base := encoding.GetCodecV2("proto")
	if base == nil {
		// No proto codec registered at all: nothing to delegate to, so leave
		// gRPC alone rather than breaking every RPC to gain a hexdump.
		return
	}
	if _, already := base.(rawAwareCodec); already {
		return
	}
	encoding.RegisterCodecV2(rawAwareCodec{base: base})
}
