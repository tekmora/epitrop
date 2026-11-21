package supervisor

import (
	"path/filepath"
	"testing"

	"github.com/tekmora/epitrop/internal/config"
)

type rotationRetryCase struct {
	name        string
	keepFiles   int
	planted     []indexedLogBody
	wantBackups []indexedLogBody
	wantMissing int
}

type indexedLogBody struct {
	index int
	body  string
}

func TestLogRotationGapAwareRetry(t *testing.T) {
	cases := []rotationRetryCase{
		{
			name:        "retry_after_failed_active_rename",
			keepFiles:   2,
			planted:     []indexedLogBody{{2, "kept-backup-B\n"}},
			wantBackups: []indexedLogBody{{1, "planted-active-line\n"}, {2, "kept-backup-B\n"}},
			wantMissing: 3,
		},
		{
			name:        "gap_in_the_middle",
			keepFiles:   3,
			planted:     []indexedLogBody{{1, "backup-X\n"}, {3, "backup-Z\n"}},
			wantBackups: []indexedLogBody{{1, "planted-active-line\n"}, {2, "backup-X\n"}, {3, "backup-Z\n"}},
			wantMissing: 4,
		},
		{
			name:        "full_set_drops_only_oldest",
			keepFiles:   2,
			planted:     []indexedLogBody{{1, "backup-X\n"}, {2, "backup-Y\n"}},
			wantBackups: []indexedLogBody{{1, "planted-active-line\n"}, {2, "backup-X\n"}},
			wantMissing: 3,
		},
		{
			name:        "keep_files_one_without_first_backup",
			keepFiles:   1,
			wantBackups: []indexedLogBody{{1, "planted-active-line\n"}},
			wantMissing: 2,
		},
	}
	for i := 0; i < len(cases); i++ {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			runRotationRetryCase(t, tc)
		})
	}
}

func runRotationRetryCase(t *testing.T, tc rotationRetryCase) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "job.log")
	mustWriteRawFile(t, logPath, "planted-active-line\n")
	for i := 0; i < len(tc.planted); i++ {
		mustWriteRawFile(t, rotatedLogPath(logPath, tc.planted[i].index), tc.planted[i].body)
	}
	log := mustOpenRotatingLog(t, logPath, config.JobLogRotation{MaxBytes: 32, KeepFiles: tc.keepFiles})
	mustWriteLine(t, log, tc.name, "fresh-trigger-line!\n")
	mustCloseRotatingLog(t, log)
	assertFileString(t, logPath, "fresh-trigger-line!\n")
	for i := 0; i < len(tc.wantBackups); i++ {
		assertFileString(t, rotatedLogPath(logPath, tc.wantBackups[i].index), tc.wantBackups[i].body)
	}
	assertPathMissing(t, rotatedLogPath(logPath, tc.wantMissing))
}
