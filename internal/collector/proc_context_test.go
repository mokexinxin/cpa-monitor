package collector

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

func TestDiskCollectionCancellationDoesNotWaitForBlockedStatFS(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	collector := testProcCollector(t, StatFSFunc(func(string) (StatFSInfo, error) {
		close(started)
		<-release
		return StatFSInfo{Blocks: 10, BlocksFree: 5, BlockSize: 1}, nil
	}))
	collector.statFSTimeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := collector.Disks(ctx)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Disks() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Disks() did not return after context cancellation")
	}
	close(release)
}

func TestDiskCollectionTimeoutReusesOutstandingStatFSCall(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	collector := testProcCollector(t, StatFSFunc(func(string) (StatFSInfo, error) {
		calls.Add(1)
		<-release
		return StatFSInfo{Blocks: 10, BlocksFree: 5, BlockSize: 1}, nil
	}))
	collector.statFSTimeout = 5 * time.Millisecond

	for cycle := 0; cycle < 2; cycle++ {
		batch, err := collector.Disks(context.Background())
		if err == nil || batch.Complete {
			t.Fatalf("cycle %d: Disks() = %+v, %v, want timeout", cycle, batch, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("blocked StatFS calls = %d, want 1", got)
	}
	close(release)
}

func testProcCollector(t *testing.T, statFS StatFS) *procCollector {
	t.Helper()
	files := fstest.MapFS{
		mountInfoPath: &fstest.MapFile{Data: []byte("36 25 0:32 / / rw - ext4 /dev/root rw\n")},
	}
	return NewProcCollector(files, statFS).(*procCollector)
}
