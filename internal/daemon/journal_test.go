package daemon

import (
	"bytes"
	"github.com/zhiylee/ssh-use/internal/protocol"
	"testing"
)

func TestJournalReportsExpiredCursorAndCompletion(t *testing.T) {
	j := newJournal()
	chunk := bytes.Repeat([]byte{1}, protocol.ChunkSize)
	for i := 0; i < 40; i++ {
		j.publish(protocol.Chunk("stdout_chunk", "job", int64(i+1), chunk))
	}
	if _, gap, _, _ := j.read(0); !gap {
		t.Fatal("silently skipped output before retained window")
	}
	cursor := j.cursor()
	j.publish(protocol.Message{Type: "final", ID: "job", OK: true})
	j.finish()
	messages, gap, done, _ := j.read(cursor)
	if gap || !done || len(messages) != 1 || messages[0].Type != "final" {
		t.Fatalf("final replay: %#v gap=%v done=%v", messages, gap, done)
	}
	if j.bytes > 4*1024*1024 {
		t.Fatal("journal exceeded byte budget")
	}
}
