package audit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhiylee/ssh-use/internal/model"
)

func TestStoreSaveRecentAndUpdate(t *testing.T) {
	t.Setenv("SSH_USE_DATA_DIR", t.TempDir())
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

func TestRestartMarksOnlyUnfinishedTasksUnknown(t *testing.T) {
	t.Setenv("SSH_USE_DATA_DIR", t.TempDir())
	ctx := context.Background()
	store, err := Open(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []model.Status{model.StatusRunning, model.StatusPendingApproval, model.StatusDone} {
		if err := store.Save(ctx, model.CommandRecord{ID: string(status), CreatedAt: time.Now(), Host: "h", Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	if fresh, err := store.ClaimRequest(ctx, "request", "digest"); err != nil || !fresh {
		t.Fatalf("claim: %t %v", fresh, err)
	}
	store.Close()
	store, err = Open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.MarkInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []model.Status{model.StatusRunning, model.StatusPendingApproval, model.StatusDone} {
		rec, err := store.Get(ctx, string(id))
		if err != nil {
			t.Fatal(err)
		}
		want := model.StatusUnknown
		if id == model.StatusDone {
			want = model.StatusDone
		}
		if rec == nil || rec.Status != want {
			t.Fatalf("%s: %#v", id, rec)
		}
	}
	if fresh, err := store.ClaimRequest(ctx, "request", "digest"); err != nil || fresh {
		t.Fatalf("reclaim: %t %v", fresh, err)
	}
	if _, err := store.ClaimRequest(ctx, "request", "other"); err == nil {
		t.Fatal("accepted conflicting request")
	}
}

func TestOpenMigratesLegacyErrorColumn(t *testing.T) {
	t.Setenv("SSH_USE_DATA_DIR", t.TempDir())
	ctx := context.Background()
	store, err := Open(true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rec := model.CommandRecord{
		ID:              "legacy",
		CreatedAt:       time.Now(),
		Host:            "prod1",
		Command:         "uptime",
		DisplayCommand:  "uptime",
		Status:          model.StatusRejected,
		SSHUseErrorCode: "approval_rejected",
	}
	if err := store.Save(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE commands RENAME COLUMN ssh_use_error_code TO agent_ssh_error_code`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(true)
	if err != nil {
		t.Fatal(err)
	}
	recent, err := store.Recent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].ID != rec.ID || recent[0].Command != rec.Command || recent[0].SSHUseErrorCode != rec.SSHUseErrorCode {
		t.Fatalf("migrated records = %#v", recent)
	}
	rec.SSHUseErrorCode = "cancelled"
	if err := store.Save(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(true)
	if err != nil {
		t.Fatal(err)
	}
	recent, err = store.Recent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].SSHUseErrorCode != "cancelled" {
		t.Fatalf("updated records = %#v", recent)
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
	if !truncated || !strings.Contains(out, "ssh-use truncated") || len(out) > 150 {
		t.Fatalf("unexpected truncate result len=%d truncated=%v out=%q", len(out), truncated, out)
	}
	small, truncated := LimitOutput("abcdef", 3)
	if !truncated || small != "abc" {
		t.Fatalf("small truncate = %q %v", small, truncated)
	}
}

func TestDeleteOlderThan(t *testing.T) {
	t.Setenv("SSH_USE_DATA_DIR", t.TempDir())
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
