package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/zhiylee/ssh-use/internal/client"
	"github.com/zhiylee/ssh-use/internal/config"
	"github.com/zhiylee/ssh-use/internal/protocol"
)

var managementRequestFn = client.Request

func RunHosts(args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	op := args[0]
	if op == "ls" {
		op = "list"
	}
	if op == "rm" || op == "remove" {
		op = "delete"
	}
	if op != "list" && op != "get" && op != "add" && op != "update" && op != "delete" {
		fmt.Fprintln(stderr, "usage: ssh-use hosts <list|get|add|update|delete> [name] [options]")
		return 2
	}
	args = args[1:]
	name := ""
	if op != "list" {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			fmt.Fprintln(stderr, "ssh-use: host name is required before options")
			return 2
		}
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("hosts "+op, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "JSON output")
	addr := fs.String("addr", "", "SSH address")
	user := fs.String("user", "", "SSH username (empty inherits default)")
	port := fs.Int("port", 0, "SSH port (0 inherits default)")
	key := fs.String("key", "", "private key path on the execution server")
	tags := fs.String("tags", "", "comma-separated tags (empty clears)")
	expected := fs.String("if-revision", "", "require this configuration revision")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	mutate := op == "add" || op == "update" || op == "delete"
	fields := set["addr"] || set["user"] || set["port"] || set["key"] || set["tags"]
	if (op == "add" && (!set["addr"] || *addr == "")) || (op == "update" && !fields) || ((op == "list" || op == "get" || op == "delete") && fields) || (!mutate && set["if-revision"]) {
		fmt.Fprintln(stderr, "ssh-use: add requires --addr; update requires at least one field; field flags only apply to add/update")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req := protocol.Message{Type: "hosts." + op, Host: name, ConfigRevision: *expected}
	if mutate && req.ConfigRevision == "" {
		resp, err := managementRequestFn(ctx, protocol.Message{Type: "hosts.list"})
		if err != nil {
			fmt.Fprintf(stderr, "ssh-use: %v\n", err)
			return 1
		}
		req.ConfigRevision = resp.ConfigRevision
	}
	parsedTags := []string{}
	if *tags != "" {
		for _, tag := range strings.Split(*tags, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				parsedTags = append(parsedTags, tag)
			}
		}
	}
	if op == "add" {
		req.HostConfig = &config.Host{Addr: *addr, User: *user, Port: *port, Key: *key, Tags: parsedTags}
	}
	if op == "update" {
		p := &protocol.HostPatch{}
		if set["addr"] {
			p.Addr = addr
		}
		if set["user"] {
			p.User = user
		}
		if set["port"] {
			p.Port = port
		}
		if set["key"] {
			p.Key = key
		}
		if set["tags"] {
			p.Tags = &parsedTags
		}
		req.HostPatch = p
	}
	resp, err := managementRequestFn(ctx, req)
	if err != nil {
		fmt.Fprintf(stderr, "ssh-use: %v\n", err)
		return 1
	}
	if *asJSON {
		return writeJSON(resp)
	}
	if mutate {
		fmt.Fprintf(stdout, "%s: %s\n", op, name)
		return 0
	}
	hosts := resp.Hosts
	if op == "get" && resp.HostConfig != nil {
		hosts = map[string]config.Host{name: *resp.HostConfig}
	}
	names := make([]string, 0, len(hosts))
	for n := range hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tADDRESS\tUSER\tPORT\tKEY\tTAGS")
	for _, n := range names {
		h := hosts[n]
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", n, h.Addr, h.User, h.Port, h.Key, strings.Join(h.Tags, ","))
	}
	_ = w.Flush()
	return 0
}

func writeJSON(value any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func RunJobs(args []string) int {
	req := protocol.Message{Type: "snapshot"}
	if len(args) == 1 && args[0] == "list" {
	} else if len(args) == 2 && args[0] == "get" {
		req = protocol.Message{Type: "command.get", ID: args[1]}
	} else {
		fmt.Fprintln(stderr, "usage: ssh-use jobs list | jobs get <id> (JSON output)")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := managementRequestFn(ctx, req)
	if err != nil {
		fmt.Fprintf(stderr, "ssh-use: %v\n", err)
		return 1
	}
	if req.Type == "snapshot" {
		return writeJSON(resp.Commands)
	}
	return writeJSON(resp.Record)
}
