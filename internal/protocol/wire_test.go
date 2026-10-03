package protocol

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/zhiylee/ssh-use/internal/config"
	"github.com/zhiylee/ssh-use/internal/model"
	"github.com/zhiylee/ssh-use/internal/rpcpb"
	"google.golang.org/protobuf/proto"
)

func TestProtobufPreservesBinaryAndOptionalFields(t *testing.T) {
	data := bytes.Repeat([]byte{0, 255, 128, '\n'}, ChunkSize/4)
	empty := ""
	zero := 0
	tags := []string{}
	want := Chunk("stdout_chunk", "job", 9, data)
	want.Cursor = 12
	want.HostConfig = &config.Host{Addr: "127.0.0.1", User: "root", Port: 22, Tags: []string{"prod"}}
	want.HostPatch = &HostPatch{Addr: &empty, Port: &zero, Tags: &tags}
	want.Record = &model.CommandRecord{ID: "job", CreatedAt: time.Now().UTC(), RemoteExitCode: &zero, Stdout: string(data), Status: model.StatusDone, PolicyDecision: &model.PolicyDecision{Action: model.ActionAllow, Risk: model.RiskLow}}
	wire, err := proto.Marshal(ToWireMessage(&want))
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) > 2*len(data)+2048 {
		t.Fatalf("binary unexpectedly expanded: %d", len(wire))
	}
	var p rpcpb.Message
	if err := proto.Unmarshal(wire, &p); err != nil {
		t.Fatal(err)
	}
	got := FromWireMessage(&p)
	if !reflect.DeepEqual(want, *got) {
		t.Fatal("protobuf lost binary data, field presence, time, or policy metadata")
	}
}
func TestLocalJSONStillPreservesBinaryChunks(t *testing.T) {
	want := []byte{0, 255, 128, '\n'}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Encode(Chunk("transfer.chunk", "id", 1, want)); err != nil {
		t.Fatal(err)
	}
	var m Message
	if err := NewDecoder(&buf).Decode(&m); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeData(m)
	if err != nil || !bytes.Equal(want, got) {
		t.Fatalf("legacy local chunk: %v %v", got, err)
	}
}
func BenchmarkChunkRoundTrip(b *testing.B) {
	data := bytes.Repeat([]byte{0, 255, 128, '\n'}, ChunkSize/4)
	msg := Chunk("transfer.chunk", "job", 1, data)
	b.Run("JSONBase64", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		var buf bytes.Buffer
		for b.Loop() {
			buf.Reset()
			if err := NewEncoder(&buf).Encode(msg); err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(buf.Len()), "wire-B")
			var decoded Message
			if err := NewDecoder(&buf).Decode(&decoded); err != nil {
				b.Fatal(err)
			}
			if _, err := DecodeData(decoded); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Protobuf", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		p := ToWireMessage(&msg)
		for b.Loop() {
			wire, err := proto.Marshal(p)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(len(wire)), "wire-B")
			var decoded rpcpb.Message
			if err := proto.Unmarshal(wire, &decoded); err != nil {
				b.Fatal(err)
			}
			if _, err := DecodeData(*FromWireMessage(&decoded)); err != nil {
				b.Fatal(err)
			}
		}
	})
}
