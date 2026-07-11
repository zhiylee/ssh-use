package protocol

import (
	"bytes"
	"testing"
)

func TestChunkBase64RoundTrip(t *testing.T) {
	original := []byte{0, 1, 2, 'h', 'i', 255}
	msg := Chunk("stdout_chunk", "cmd", 7, original)
	if msg.Encoding != "base64" || msg.Seq != 7 || msg.ID != "cmd" {
		t.Fatalf("bad chunk metadata: %#v", msg)
	}
	decoded, err := DecodeData(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, original) {
		t.Fatalf("decoded = %v", decoded)
	}
}

func TestJSONLinesRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.Encode(Message{Type: "final", AgentSSHErrorCode: "policy_blocked"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"agent_ssh_error_code":"policy_blocked"`)) {
		t.Fatalf("encoded message = %s", buf.Bytes())
	}
	dec := NewDecoder(&buf)
	var got Message
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "final" || got.AgentSSHErrorCode != "policy_blocked" {
		t.Fatalf("got %#v", got)
	}
}

func TestDecodePlainData(t *testing.T) {
	got, err := DecodeData(Message{Data: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "plain" {
		t.Fatalf("got %q", got)
	}
}
