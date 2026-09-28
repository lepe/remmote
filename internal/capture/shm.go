package capture

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// SHMSegment owns one System V shared memory segment attached by this
// process and shared with the X server. IPC_RMID is issued immediately
// after attach so the kernel destroys the segment automatically once both
// sides have detached — even if we crash.
type SHMSegment struct {
	id  int
	mem []byte
}

// NewSHMSegment creates a segment of exactly size bytes (the kernel
// rounds up to page size).
func NewSHMSegment(size int) (*SHMSegment, error) {
	id, err := unix.SysvShmGet(unix.IPC_PRIVATE, size, unix.IPC_CREAT|unix.IPC_EXCL|0o600)
	if err != nil {
		return nil, fmt.Errorf("shmget(%d bytes): %w", size, err)
	}
	mem, err := unix.SysvShmAttach(id, 0, 0)
	if err != nil {
		_, _ = unix.SysvShmCtl(id, unix.IPC_RMID, nil)
		return nil, fmt.Errorf("shmat: %w", err)
	}
	if _, err := unix.SysvShmCtl(id, unix.IPC_RMID, nil); err != nil {
		_ = unix.SysvShmDetach(mem)
		return nil, fmt.Errorf("shmctl(IPC_RMID): %w", err)
	}
	return &SHMSegment{id: id, mem: mem}, nil
}

// ID is the System V shmid, for shm.Attach.
func (s *SHMSegment) ID() int { return s.id }

// Mem is the attached segment memory.
func (s *SHMSegment) Mem() []byte { return s.mem }

// Close detaches our view of the segment. Only valid after the X server
// has detached (shm.Detach) — or harmless at process exit, since the
// early IPC_RMID already guaranteed reclamation.
func (s *SHMSegment) Close() error {
	return unix.SysvShmDetach(s.mem)
}
