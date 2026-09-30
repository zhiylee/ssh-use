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
	if err := enc.Encode(Message{Type: "final", SSHUseErrorCode: "policy_blocked"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"ssh_use_error_code":"policy_blocked"`)) {
		t.Fatalf("encoded message = %s", buf.Bytes())
	}
	dec := NewDecoder(&buf)
	var got Message
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "final" || got.SSHUseErrorCode != "policy_blocked" {
		t.Fatalf("got %#v", got)
	}
}

func TestRuntimeSettingsRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := &RuntimeSettings{Generation: 7, Mode: "auto", ConfirmRisks: []string{}, Theme: "dark", FocusPending: true}
	if err := NewEncoder(&buf).Encode(Message{Type: "snapshot", RuntimeSettings: want}); err != nil {
		t.Fatal(err)
	}
	var got Message
	if err := NewDecoder(&buf).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.RuntimeSettings == nil || got.RuntimeSettings.Generation != 7 || got.RuntimeSettings.Mode != "auto" || got.RuntimeSettings.ConfirmRisks == nil || len(got.RuntimeSettings.ConfirmRisks) != 0 {
		t.Fatalf("runtime settings = %#v", got.RuntimeSettings)
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
