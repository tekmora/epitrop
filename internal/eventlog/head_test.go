package eventlog

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestHeadSeqEmptyDirReturnsZero(t *testing.T) {
	_, signer, _ := testLog(t)
	dir := t.TempDir()
	seq, err := HeadSeq(testContext(t), dir, signer.PublicKey())
	if err != nil {
		t.Fatalf("HeadSeq on empty dir = %v, want nil", err)
	}
	if seq != 0 {
		t.Fatalf("HeadSeq on empty dir = %d, want 0", seq)
	}
}

func TestHeadSeqEventsDirWithoutHeadReturnsZero(t *testing.T) {
	_, signer, _ := testLog(t)
	dir := t.TempDir()
	if err := os.MkdirAll(eventsDir(dir), 0o750); err != nil {
		t.Fatalf("MkdirAll = %v", err)
	}
	seq, err := HeadSeq(testContext(t), dir, signer.PublicKey())
	if err != nil {
		t.Fatalf("HeadSeq on empty events dir = %v, want nil", err)
	}
	if seq != 0 {
		t.Fatalf("HeadSeq on empty events dir = %d, want 0", seq)
	}
}

func TestHeadSeqAfterAppendsReturnsLastSeq(t *testing.T) {
	dir, pub, _ := fileLog(t, 5)
	seq, err := HeadSeq(testContext(t), dir, pub)
	if err != nil {
		t.Fatalf("HeadSeq after appends = %v, want nil", err)
	}
	if seq != 5 {
		t.Fatalf("HeadSeq = %d, want 5", seq)
	}
}

func TestHeadSeqTamperedHeadFails(t *testing.T) {
	dir, pub, _ := fileLog(t, 3)
	headPath := filepath.Join(eventsDir(dir), "HEAD.json")
	replaceInFile(t, headPath, `"seq":3`, `"seq":4`)
	_, err := HeadSeq(testContext(t), dir, pub)
	if err == nil {
		t.Fatal("HeadSeq with tampered HEAD succeeded, want verification failure")
	}
}

func TestHeadSeqChecksInputs(t *testing.T) {
	_, signer, _ := testLog(t)
	pub := signer.PublicKey()
	ctx := testContext(t)

	if _, err := HeadSeq(ctx, "", pub); err == nil {
		t.Fatal("HeadSeq with empty dir succeeded, want error")
	}
	if _, err := HeadSeq(ctx, t.TempDir(), "invalid-hex"); err == nil {
		t.Fatal("HeadSeq with invalid pub key succeeded, want error")
	}
	cancCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := HeadSeq(cancCtx, t.TempDir(), pub); err == nil {
		t.Fatal("HeadSeq with canceled context succeeded, want error")
	}
}
