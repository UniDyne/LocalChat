package store

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/marcboeker/go-duckdb"
)

func TestSetPlanRepeatedUpdatesSameSeqs(t *testing.T) {
	f, err := os.CreateTemp("", "plan-repro-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	os.Remove(path)
	defer os.Remove(path)

	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE plan_steps (
		session_id TEXT NOT NULL,
		seq        INTEGER NOT NULL,
		content    TEXT NOT NULL,
		status     TEXT NOT NULL DEFAULT 'pending',
		updated_at TIMESTAMP NOT NULL,
		PRIMARY KEY (session_id, seq)
	);`); err != nil {
		t.Fatal(err)
	}

	s := &Store{db: db}
	sessionID := "test-session"

	steps := []PlanStep{
		{Seq: 1, Content: "step one", Status: "pending"},
		{Seq: 2, Content: "step two", Status: "pending"},
		{Seq: 3, Content: "step three", Status: "pending"},
	}
	if err := s.SetPlan(sessionID, steps); err != nil {
		t.Fatalf("initial SetPlan: %v", err)
	}

	// Simulate the model marking step 1 in_progress, same (session_id, seq)
	// pairs resent every time — this is exactly the pattern that hit the
	// duplicate-key error against the old delete-then-reinsert version.
	steps[0].Status = "in_progress"
	if err := s.SetPlan(sessionID, steps); err != nil {
		t.Fatalf("second SetPlan (mark in_progress): %v", err)
	}

	steps[0].Status = "completed"
	steps[1].Status = "in_progress"
	if err := s.SetPlan(sessionID, steps); err != nil {
		t.Fatalf("third SetPlan (mark completed + next in_progress): %v", err)
	}

	got, err := s.GetPlan(sessionID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(got))
	}
	if got[0].Status != "completed" || got[1].Status != "in_progress" || got[2].Status != "pending" {
		t.Fatalf("unexpected statuses: %+v", got)
	}

	// Shrinking the plan should trim the trailing row.
	shrunk := []PlanStep{
		{Seq: 1, Content: "step one", Status: "completed"},
		{Seq: 2, Content: "step two", Status: "completed"},
	}
	if err := s.SetPlan(sessionID, shrunk); err != nil {
		t.Fatalf("shrink SetPlan: %v", err)
	}
	got, err = s.GetPlan(sessionID)
	if err != nil {
		t.Fatalf("GetPlan after shrink: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 steps after shrink, got %d: %+v", len(got), got)
	}
}
