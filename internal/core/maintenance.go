package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// MaintenancePolicy describes the controller data that is safe to retire.
// Delivery inbox rows and instance records are intentionally excluded because
// they are replay/ownership evidence and require an explicit retirement fence.
type MaintenancePolicy struct {
	Before           time.Time `json:"before"`
	MinimumAuditRows int64     `json:"minimumAuditRows"`
}

// MaintenanceReport makes both reclaimed and deliberately retained state
// visible to operators. A dry run returns the same candidate counts without
// modifying the database.
type MaintenanceReport struct {
	Applied               bool      `json:"applied"`
	Cutoff                time.Time `json:"cutoff"`
	MinimumAuditRows      int64     `json:"minimumAuditRows"`
	ExpiredPlans          int64     `json:"expiredPlans"`
	UnreferencedInvites   int64     `json:"unreferencedInvites"`
	OldAuditRows          int64     `json:"oldAuditRows"`
	ProtectedInstances    int64     `json:"protectedInstances"`
	ProtectedInboxRows    int64     `json:"protectedInboxRows"`
	ProtectedEnrollments  int64     `json:"protectedEnrollments"`
	DatabaseBytesBefore   int64     `json:"databaseBytesBefore"`
	DatabaseBytesAfter    int64     `json:"databaseBytesAfter"`
	WALCheckpointComplete bool      `json:"walCheckpointComplete"`
	VacuumComplete        bool      `json:"vacuumComplete"`
}

func (p MaintenancePolicy) validate(now time.Time) error {
	if p.Before.IsZero() || p.Before.After(now.Add(-24*time.Hour)) {
		return errors.New("maintenance cutoff must be at least 24 hours in the past")
	}
	if p.MinimumAuditRows < 1 || p.MinimumAuditRows > 1_000_000 {
		return errors.New("minimum audit rows must be between 1 and 1000000")
	}
	return nil
}

func (s *Store) databaseFootprint() (int64, error) {
	var total int64
	for _, name := range []string{"controller.db", "controller.db-wal", "controller.db-shm"} {
		st, err := os.Lstat(filepath.Join(s.Dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if !st.Mode().IsRegular() {
			return 0, fmt.Errorf("database component %s is not a regular file", name)
		}
		total += st.Size()
	}
	return total, nil
}

func maintenanceCountRow(ctx context.Context, q rowReader, query string, args ...any) (int64, error) {
	var count int64
	if err := q.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func maintenanceReport(ctx context.Context, q rowReader, policy MaintenancePolicy) (MaintenanceReport, error) {
	cutoff := policy.Before.UTC().Unix()
	report := MaintenanceReport{Cutoff: policy.Before.UTC(), MinimumAuditRows: policy.MinimumAuditRows}
	var err error
	if report.ExpiredPlans, err = maintenanceCountRow(ctx, q, "SELECT COUNT(*) FROM plans WHERE expires < ?", cutoff); err != nil {
		return report, err
	}
	if report.UnreferencedInvites, err = maintenanceCountRow(ctx, q, `SELECT COUNT(*) FROM invites
		WHERE expires < ? AND NOT EXISTS (SELECT 1 FROM enrollments WHERE enrollments.invite=invites.id)`, cutoff); err != nil {
		return report, err
	}
	if report.OldAuditRows, err = maintenanceCountRow(ctx, q, `SELECT COUNT(*) FROM audit
		WHERE at < ? AND id NOT IN (SELECT id FROM audit ORDER BY id DESC LIMIT ?)`, cutoff, policy.MinimumAuditRows); err != nil {
		return report, err
	}
	if report.ProtectedInstances, err = maintenanceCountRow(ctx, q, "SELECT COUNT(*) FROM instances"); err != nil {
		return report, err
	}
	if report.ProtectedInboxRows, err = maintenanceCountRow(ctx, q, "SELECT COUNT(*) FROM inbox"); err != nil {
		return report, err
	}
	if report.ProtectedEnrollments, err = maintenanceCountRow(ctx, q, "SELECT COUNT(*) FROM enrollments"); err != nil {
		return report, err
	}
	return report, nil
}

func maintenancePostCommitError(report MaintenanceReport, err error) error {
	var fault *Error
	if errors.As(err, &fault) {
		return Fail(fault.Code, fault.Message, map[string]any{"report": report, "causeDetails": fault.Details})
	}
	return Fail("OPERATION_FAILED", err.Error(), report)
}

// Maintain retires only bounded, reconstructible controller bookkeeping:
// expired plans, expired invitations that are not referenced by an enrollment,
// and old audit rows beyond an operator-selected retained tail. It never deletes
// instances, message deduplication rows, enrollments, images, disks or unknown
// host state. When apply is true the caller must hold the controller process
// lock so checkpoint and compaction run against an offline controller.
func (s *Store) Maintain(ctx context.Context, policy MaintenancePolicy, apply bool) (MaintenanceReport, error) {
	if err := policy.validate(s.Now().UTC()); err != nil {
		return MaintenanceReport{}, err
	}
	before, err := s.databaseFootprint()
	if err != nil {
		return MaintenanceReport{}, err
	}

	report, err := maintenanceReport(ctx, s.DB, policy)
	if err != nil {
		return report, err
	}
	report.DatabaseBytesBefore = before
	report.DatabaseBytesAfter = before
	if !apply {
		return report, nil
	}

	err = s.transaction(ctx, func(tx *sql.Tx) error {
		cutoff := policy.Before.UTC().Unix()
		if _, err := tx.ExecContext(ctx, "DELETE FROM plans WHERE expires < ?", cutoff); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM invites
			WHERE expires < ? AND NOT EXISTS (SELECT 1 FROM enrollments WHERE enrollments.invite=invites.id)`, cutoff); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM audit
			WHERE at < ? AND id NOT IN (SELECT id FROM audit ORDER BY id DESC LIMIT ?)`, cutoff, policy.MinimumAuditRows); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO audit(at,event,target) VALUES(?,?,?)", s.Now().UTC().Unix(), "maintenance.compact", policy.Before.UTC().Format(time.RFC3339))
		return err
	})
	if err != nil {
		return report, err
	}
	// The deletions and audit record are durable now. Later checkpoint, VACUUM,
	// or accounting failures must not make the operation look unapplied.
	report.Applied = true

	var busy, logFrames, checkpointedFrames int
	if err = s.DB.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		return report, maintenancePostCommitError(report, err)
	}
	if busy != 0 {
		return report, maintenancePostCommitError(report, Fail("MAINTENANCE_BUSY", "database checkpoint could not obtain exclusive access", map[string]int{"logFrames": logFrames, "checkpointedFrames": checkpointedFrames}))
	}
	report.WALCheckpointComplete = true
	if _, err = s.DB.ExecContext(ctx, "VACUUM"); err != nil {
		return report, maintenancePostCommitError(report, err)
	}
	report.VacuumComplete = true
	report.DatabaseBytesAfter, err = s.databaseFootprint()
	if err != nil {
		return report, maintenancePostCommitError(report, err)
	}
	return report, nil
}
