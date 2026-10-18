package eventlog

import (
	"context"
	"errors"
	"os"
)

// HeadSeq returns the sequence number of verified HEAD.json in dir's event log, or 0
// when the events directory or HEAD.json does not exist.
func HeadSeq(ctx context.Context, dir string, pubKeyHex string) (uint64, error) {
	if err := readyContext(ctx, "head"); err != nil {
		return 0, err
	}
	if dir == "" {
		return 0, errors.New("eventlog: empty directory")
	}
	if err := checkPublicKeyHex(pubKeyHex); err != nil {
		return 0, err
	}
	if _, err := os.Stat(eventsDir(dir)); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	h, exists, err := readDecodedHead(eventsDir(dir), pubKeyHex)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	return h.Seq, nil
}
