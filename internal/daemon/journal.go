package daemon

import (
	"github.com/zhiylee/ssh-use/internal/protocol"
	"sync"
)

// Journals are bounded in bytes and messages. A reader that falls behind must
// explicitly refresh state or report an output gap, never silently skip bytes.
type messageJournal struct {
	mu       sync.Mutex
	messages []protocol.Message
	bytes    int
	next     uint64
	done     bool
	changed  chan struct{}
}

func newJournal() *messageJournal { return &messageJournal{changed: make(chan struct{})} }
func messageBytes(m protocol.Message) int {
	n := 256 + len(m.Payload) + len(m.Data) + len(m.Command) + len(m.Error)
	if m.Record != nil {
		n += len(m.Record.Stdout) + len(m.Record.Stderr) + len(m.Record.Command) + 1024
	}
	return n
}
func (j *messageJournal) publish(m protocol.Message) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if m.Revision > 0 {
		j.next = m.Revision
	} else {
		j.next++
	}
	m.Cursor = j.next
	j.messages = append(j.messages, m)
	j.bytes += messageBytes(m)
	for len(j.messages) > 1 && (len(j.messages) > 1024 || j.bytes > 4*1024*1024) {
		j.bytes -= messageBytes(j.messages[0])
		j.messages[0] = protocol.Message{}
		j.messages = j.messages[1:]
	}
	close(j.changed)
	j.changed = make(chan struct{})
}
func (j *messageJournal) finish() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.done = true
	close(j.changed)
	j.changed = make(chan struct{})
}
func (j *messageJournal) read(after uint64) ([]protocol.Message, bool, bool, <-chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if after > j.next || (len(j.messages) == 0 && after < j.next) || (len(j.messages) > 0 && after < j.messages[0].Cursor-1) {
		return nil, true, j.done, j.changed
	}
	var result []protocol.Message
	for _, m := range j.messages {
		if m.Cursor > after {
			result = append(result, m)
		}
	}
	return result, false, j.done, j.changed
}
func (j *messageJournal) cursor() uint64 { j.mu.Lock(); defer j.mu.Unlock(); return j.next }
