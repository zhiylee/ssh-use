package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/zhiylee/ssh-use/internal/config"
	"github.com/zhiylee/ssh-use/internal/protocol"
)

func TestHostsCRUDRequests(t *testing.T) {
	old := managementRequestFn
	oldOut, oldErr := stdout, stderr
	t.Cleanup(func() { managementRequestFn = old; stdout = oldOut; stderr = oldErr })
	var out, errOut bytes.Buffer
	stdout = &out
	stderr = &errOut
	for _, tc := range []struct {
		args  []string
		check func(protocol.Message) bool
	}{
		{[]string{"add", "prod", "--addr", "192.0.2.1", "--port", "2222", "--tags", "prod, blue"}, func(m protocol.Message) bool {
			return m.Type == "hosts.add" && m.HostConfig.Addr == "192.0.2.1" && m.HostConfig.Port == 2222 && len(m.HostConfig.Tags) == 2
		}},
		{[]string{"update", "prod", "--user", "", "--tags", ""}, func(m protocol.Message) bool {
			return m.Type == "hosts.update" && m.HostPatch.User != nil && *m.HostPatch.User == "" && m.HostPatch.Tags != nil && len(*m.HostPatch.Tags) == 0 && m.HostPatch.Addr == nil
		}},
		{[]string{"delete", "prod"}, func(m protocol.Message) bool { return m.Type == "hosts.delete" }},
	} {
		calls := 0
		managementRequestFn = func(ctx context.Context, m protocol.Message) (protocol.Message, error) {
			calls++
			if calls == 1 {
				if m.Type != "hosts.list" {
					t.Fatal(m)
				}
				return protocol.Message{OK: true, ConfigRevision: "v1"}, nil
			}
			if m.Host != "prod" || m.ConfigRevision != "v1" || !tc.check(m) {
				t.Fatalf("bad request: %#v", m)
			}
			return protocol.Message{OK: true}, nil
		}
		if code := RunHosts(tc.args); code != 0 || calls != 2 {
			t.Fatalf("%v code=%d calls=%d err=%s", tc.args, code, calls, errOut.String())
		}
	}
	managementRequestFn = func(context.Context, protocol.Message) (protocol.Message, error) {
		return protocol.Message{OK: true, Hosts: map[string]config.Host{"prod": {Addr: "192.0.2.1"}}, ConfigRevision: "v1"}, nil
	}
	out.Reset()
	if code := RunHosts([]string{"list", "--json"}); code != 0 || !strings.Contains(out.String(), `"addr": "192.0.2.1"`) {
		t.Fatalf("list: %d %s", code, out.String())
	}
}

func TestHostsRejectsInvalidArgumentsBeforeConnection(t *testing.T) {
	old := managementRequestFn
	oldErr := stderr
	t.Cleanup(func() { managementRequestFn = old; stderr = oldErr })
	stderr = &bytes.Buffer{}
	managementRequestFn = func(context.Context, protocol.Message) (protocol.Message, error) {
		t.Fatal("unexpected connection")
		return protocol.Message{}, nil
	}
	for _, args := range [][]string{{"add", "h"}, {"update", "h"}, {"delete"}, {"list", "--addr", "x"}, {"delete", "h", "--key", "x"}, {"get", "h", "extra"}} {
		if code := RunHosts(args); code != 2 {
			t.Fatalf("%v: %d", args, code)
		}
	}
}
