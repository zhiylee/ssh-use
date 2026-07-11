package audit

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-ssh/internal/model"
)

func TestStoreSaveRecentAndUpdate(t *testing.T) {
	t.Setenv("AGENT_SSH_DATA_DIR", t.TempDir())
	store, err := Open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	exit := 2
	rec := model.CommandRecord{
		ID:             "cmd1",
		CreatedAt:      time.Now(),
		Host:           "h",
		Command:        "uptime",
		DisplayCommand: "uptime",
		Status:         model.StatusFailed,
		Risk:           model.RiskLow,
		PolicyAction:   model.ActionAllow,
		RemoteExitCode: &exit,
		Stdout:         "out",
	}
	if err := store.Save(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	rec.Status = model.StatusDone
	rec.Stdout = "updated"
	if err := store.Save(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	recent, err := store.Recent(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Status != model.StatusDone || recent[0].Stdout != "updated" || recent[0].RemoteExitCode == nil || *recent[0].RemoteExitCode != 2 {
		t.Fatalf("recent = %#v", recent)
	}
}

func TestDisabledStoreIsNoop(t *testing.T) {
	store, err := Open(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), model.CommandRecord{}); err != nil {
		t.Fatal(err)
	}
	recent, err := store.Recent(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if recent != nil {
		t.Fatalf("expected nil recent for disabled store, got %#v", recent)
	}
}

func TestLimitOutput(t *testing.T) {
	out, truncated := LimitOutput("hello", 10)
	if truncated || out != "hello" {
		t.Fatalf("unexpected no truncate result %q %v", out, truncated)
	}
	long := strings.Repeat("a", 300)
	out, truncated = LimitOutput(long, 150)
	if !truncated || !strings.Contains(out, "agent-ssh truncated") || len(out) > 150 {
		t.Fatalf("unexpected truncate result len=%d truncated=%v out=%q", len(out), truncated, out)
	}
	small, truncated := LimitOutput("abcdef", 3)
	if !truncated || small != "abc" {
		t.Fatalf("small truncate = %q %v", small, truncated)
	}
}

func TestDeleteOlderThan(t *testing.T) {
	t.Setenv("AGENT_SSH_DATA_DIR", t.TempDir())
	store, err := Open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	old := model.CommandRecord{ID: "old", CreatedAt: time.Now().Add(-48 * time.Hour), Host: "h", Command: "old", DisplayCommand: "old", Status: model.StatusDone}
	newer := model.CommandRecord{ID: "new", CreatedAt: time.Now(), Host: "h", Command: "new", DisplayCommand: "new", Status: model.StatusDone}
	if err := store.Save(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteOlderThan(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d", deleted)
	}
	recent, err := store.Recent(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].ID != "new" {
		t.Fatalf("recent=%#v", recent)
	}
}
