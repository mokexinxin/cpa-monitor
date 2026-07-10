package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"
)

const (
	memInfoPath   = "proc/meminfo"
	mountInfoPath = "proc/self/mountinfo"
	tcp4Path      = "proc/net/tcp"
	tcp6Path      = "proc/net/tcp6"

	defaultStatFSTimeout = 10 * time.Second
)

type procCollector struct {
	files         fs.FS
	statFS        StatFS
	statFSTimeout time.Duration
	statMu        sync.Mutex
	statCalls     map[string]*statCall
}

type statCall struct {
	done   chan struct{}
	result statResult
}

type statResult struct {
	info StatFSInfo
	err  error
}

func (c *procCollector) Memory(ctx context.Context) (MemoryUsage, error) {
	if err := contextError(ctx); err != nil {
		return MemoryUsage{}, err
	}
	file, err := c.open(memInfoPath)
	if err != nil {
		return MemoryUsage{}, err
	}
	defer file.Close()
	usage, err := ParseMemInfo(file)
	if err != nil {
		return MemoryUsage{}, fmt.Errorf("parse /%s: %w", memInfoPath, err)
	}
	if err := contextError(ctx); err != nil {
		return MemoryUsage{}, err
	}
	return usage, nil
}

func (c *procCollector) Disks(ctx context.Context) (DiskBatch, error) {
	if err := contextError(ctx); err != nil {
		return DiskBatch{Complete: false}, err
	}
	file, err := c.open(mountInfoPath)
	if err != nil {
		return DiskBatch{Complete: false}, err
	}
	defer file.Close()
	mounts, err := ParseMountInfo(file)
	if err != nil {
		return DiskBatch{Complete: false}, fmt.Errorf("parse /%s: %w", mountInfoPath, err)
	}
	if err := contextError(ctx); err != nil {
		return DiskBatch{Complete: false}, err
	}
	batch := c.collectDisks(ctx, mounts)
	return batch, batch.Err()
}

func (c *procCollector) collectDisks(ctx context.Context, mounts []Mount) DiskBatch {
	normalized := normalizeMounts(mounts)
	batch := DiskBatch{Disks: make([]DiskUsage, 0, len(normalized)), Complete: true}
	if c == nil || c.statFS == nil {
		batch.Complete = false
		batch.Errors = append(batch.Errors, DiskError{Err: errors.New("nil statfs implementation")})
		return batch
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for _, mount := range normalized {
		info, err := c.statFSForMount(ctx, mount.MountPoint)
		if err == nil {
			var usage DiskUsage
			usage, err = diskUsage(mount, info)
			if err == nil {
				batch.Disks = append(batch.Disks, usage)
				continue
			}
		}
		batch.Complete = false
		batch.Errors = append(batch.Errors, DiskError{MountPoint: mount.MountPoint, Err: err})
		if ctx.Err() != nil {
			return batch
		}
	}
	return batch
}

func (c *procCollector) statFSForMount(ctx context.Context, path string) (StatFSInfo, error) {
	call := c.statCall(path)
	timeout := c.statFSTimeout
	if timeout <= 0 {
		timeout = defaultStatFSTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-call.done:
		c.finishStatCall(path, call)
		return call.result.info, call.result.err
	case <-ctx.Done():
		return StatFSInfo{}, fmt.Errorf("statfs canceled: %w", ctx.Err())
	case <-timer.C:
		return StatFSInfo{}, fmt.Errorf("statfs timed out after %s", timeout)
	}
}

func (c *procCollector) statCall(path string) *statCall {
	c.statMu.Lock()
	defer c.statMu.Unlock()
	if c.statCalls == nil {
		c.statCalls = make(map[string]*statCall)
	}
	if existing := c.statCalls[path]; existing != nil {
		return existing
	}
	call := &statCall{done: make(chan struct{})}
	c.statCalls[path] = call
	go func() {
		call.result.info, call.result.err = c.statFS.StatFS(path)
		close(call.done)
	}()
	return call
}

func (c *procCollector) finishStatCall(path string, completed *statCall) {
	c.statMu.Lock()
	defer c.statMu.Unlock()
	if c.statCalls[path] == completed {
		delete(c.statCalls, path)
	}
}

func (c *procCollector) TCP(ctx context.Context, servicePort int) (TCPUsage, error) {
	if err := contextError(ctx); err != nil {
		return TCPUsage{}, err
	}
	v4, err := c.readTCP(tcp4Path, servicePort)
	if err != nil {
		return TCPUsage{}, err
	}
	if err := contextError(ctx); err != nil {
		return TCPUsage{}, err
	}
	v6, err := c.readTCP(tcp6Path, servicePort)
	if err != nil {
		return TCPUsage{}, err
	}
	if err := contextError(ctx); err != nil {
		return TCPUsage{}, err
	}
	return TCPUsage{
		TotalConnections:       v4.TotalConnections + v6.TotalConnections,
		ServicePortConnections: v4.ServicePortConnections + v6.ServicePortConnections,
	}, nil
}

func (c *procCollector) readTCP(path string, servicePort int) (TCPUsage, error) {
	file, err := c.open(path)
	if err != nil {
		return TCPUsage{}, err
	}
	defer file.Close()
	usage, err := ParseTCP(file, servicePort)
	if err != nil {
		return TCPUsage{}, fmt.Errorf("parse /%s: %w", path, err)
	}
	return usage, nil
}

func (c *procCollector) open(path string) (fs.File, error) {
	if c == nil || c.files == nil {
		return nil, fmt.Errorf("open /%s: collector filesystem is nil", path)
	}
	file, err := c.files.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open /%s: %w", path, err)
	}
	return file, nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("collect host facts: %w", err)
	}
	return nil
}
