package core

import (
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

func TestMaintenanceDryRunAndApplyPreserveReplayEvidence(t *testing.T) {
	s, _ := testStore(t)
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	old := now.Add(-90 * 24 * time.Hour).Unix()
	fresh := now.Add(-time.Hour).Unix()

	statements := []struct {
		query string
		args  []any
	}{
		{"DELETE FROM audit", nil},
		{"INSERT INTO plans(id,base,expires,payload) VALUES('old-plan',0,?,X'7B7D')", []any{old}},
		{"INSERT INTO plans(id,base,expires,payload) VALUES('fresh-plan',0,?,X'7B7D')", []any{fresh}},
		{"INSERT INTO invites(id,hash,expires) VALUES('old-unreferenced','hash',?)", []any{old}},
		{"INSERT INTO invites(id,hash,expires) VALUES('old-referenced','hash',?)", []any{old}},
		{"INSERT INTO enrollments(id,invite,name,csr,ceiling,status) VALUES('enrollment','old-referenced','node-a',X'00',X'7B7D','PendingApproval')", nil},
		{"INSERT INTO instances(id,request,payload) VALUES('0123456789abcdef0123456789abcdef','request',X'7B7D')", nil},
		{"INSERT INTO inbox(session,id,hash,created) VALUES('session',1,'hash',?)", []any{old}},
	}
	for _, statement := range statements {
		if _, err := s.DB.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := s.DB.Exec("INSERT INTO audit(at,event,target) VALUES(?,?,?)", old+int64(i), "old", "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := s.DB.Exec("INSERT INTO audit(at,event,target) VALUES(?,?,?)", fresh+int64(i), "fresh", "fixture"); err != nil {
			t.Fatal(err)
		}
	}

	policy := MaintenancePolicy{Before: now.Add(-30 * 24 * time.Hour), MinimumAuditRows: 2}
	preview, err := s.Maintain(ctx, policy, false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Applied || preview.ExpiredPlans != 1 || preview.UnreferencedInvites != 1 || preview.OldAuditRows != 5 {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if preview.ProtectedInstances != 1 || preview.ProtectedInboxRows != 1 || preview.ProtectedEnrollments != 1 {
		t.Fatalf("replay or ownership evidence not reported as protected: %+v", preview)
	}

	applied, err := s.Maintain(ctx, policy, true)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || !applied.WALCheckpointComplete || !applied.VacuumComplete {
		t.Fatalf("maintenance did not complete: %+v", applied)
	}
	for query, want := range map[string]int{
		"SELECT COUNT(*) FROM plans WHERE id='old-plan'":               0,
		"SELECT COUNT(*) FROM plans WHERE id='fresh-plan'":             1,
		"SELECT COUNT(*) FROM invites WHERE id='old-unreferenced'":     0,
		"SELECT COUNT(*) FROM invites WHERE id='old-referenced'":       1,
		"SELECT COUNT(*) FROM instances":                               1,
		"SELECT COUNT(*) FROM inbox":                                   1,
		"SELECT COUNT(*) FROM enrollments WHERE id='enrollment'":       1,
		"SELECT COUNT(*) FROM audit WHERE event='old'":                 0,
		"SELECT COUNT(*) FROM audit WHERE event='fresh'":               3,
		"SELECT COUNT(*) FROM audit WHERE event='maintenance.compact'": 1,
	} {
		var got int
		if err := s.DB.QueryRow(query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s: got %d want %d", query, got, want)
		}
	}
}

func TestMaintenanceReportsCommittedDeletionWhenCheckpointBusy(t *testing.T) {
	s, _ := testStore(t)
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	old := now.Add(-90 * 24 * time.Hour).Unix()
	if _, err := s.DB.Exec("INSERT INTO plans(id,base,expires,payload) VALUES('old-plan',0,?,X'7B7D')", old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("PRAGMA busy_timeout=100"); err != nil {
		t.Fatal(err)
	}

	u := url.URL{Scheme: "file", Path: filepath.Join(s.Dir, "controller.db")}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(100)")
	u.RawQuery = q.Encode()
	blocker, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	reader, err := blocker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	var snapshot int
	if err = reader.QueryRowContext(ctx, "SELECT COUNT(*) FROM plans").Scan(&snapshot); err != nil {
		t.Fatal(err)
	}

	report, err := s.Maintain(ctx, MaintenancePolicy{Before: now.Add(-30 * 24 * time.Hour), MinimumAuditRows: 1}, true)
	if err == nil {
		t.Fatal("checkpoint unexpectedly succeeded while a prior snapshot was open")
	}
	if !report.Applied || report.WALCheckpointComplete || report.VacuumComplete {
		t.Fatalf("committed deletion was reported incorrectly: %+v", report)
	}
	var fault *Error
	if !errors.As(err, &fault) || fault.Code != "MAINTENANCE_BUSY" {
		t.Fatalf("unexpected maintenance failure: %T %v", err, err)
	}
	details, ok := fault.Details.(map[string]any)
	if !ok {
		t.Fatalf("partial report missing from structured error: %#v", fault.Details)
	}
	partial, ok := details["report"].(MaintenanceReport)
	if !ok || !partial.Applied || partial.WALCheckpointComplete {
		t.Fatalf("structured error has wrong partial report: %#v", details)
	}
	var remaining int
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM plans WHERE id='old-plan'").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("deletion was not persisted before the checkpoint failure")
	}
}

func TestMaintenanceRejectsUnsafeCutoff(t *testing.T) {
	s, _ := testStore(t)
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	if _, err := s.Maintain(ctx, MaintenancePolicy{Before: now.Add(-time.Hour), MinimumAuditRows: 100}, false); err == nil {
		t.Fatal("unsafe recent cutoff accepted")
	}
	if _, err := s.Maintain(ctx, MaintenancePolicy{Before: now.Add(-48 * time.Hour), MinimumAuditRows: 0}, false); err == nil {
		t.Fatal("zero audit retention accepted")
	}
}
