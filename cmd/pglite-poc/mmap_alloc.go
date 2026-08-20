//go:build darwin || linux

package main

import (
	"context"

	"github.com/tetratelabs/wazero/experimental"
	"golang.org/x/sys/unix"
)

// withMemoryAllocator installs the mmap-backed linear-memory allocator.
func withMemoryAllocator(ctx context.Context) context.Context {
	return experimental.WithMemoryAllocator(ctx, mmapAllocator{})
}

// mmapAllocator backs wazero linear memory with an anonymous mmap sized to the
// module's declared max. This mirrors what native runtimes (wasmtime) do:
//   - memory.grow becomes a reslice of the same mapping (no realloc/memmove of
//     the whole linear memory — the dominant execution cost otherwise), and
//   - pages are demand-committed by the OS, so RSS tracks actual usage rather
//     than the 2GB reservation.
//
// Profiled: ~65-73% of a grow-heavy query was MemoryInstance.Grow -> memmove;
// this allocator removes it without the eager-commit cost of
// WithMemoryCapacityFromMax (which regressed the initdb PoC to ~3.4GB RSS).
type mmapAllocator struct{}

func (mmapAllocator) Allocate(capBytes, maxBytes uint64) experimental.LinearMemory {
	if maxBytes == 0 {
		maxBytes = capBytes
	}
	// Reserve the full max up front as address space; the OS commits pages
	// lazily on first touch, so this does not eagerly consume maxBytes of RAM.
	b, err := unix.Mmap(-1, 0, int(maxBytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		panic(err)
	}
	return &mmapMemory{all: b}
}

type mmapMemory struct {
	all []byte
}

// Reallocate returns a view of the reserved mapping resized to size bytes.
// Because the base address never changes, growth is a slice reslice — no copy —
// and it also satisfies the shared-memory address-stability requirement.
func (m *mmapMemory) Reallocate(size uint64) []byte {
	if size > uint64(len(m.all)) {
		return nil // cannot exceed the reserved max
	}
	return m.all[:size:len(m.all)]
}

func (m *mmapMemory) Free() {
	if m.all != nil {
		_ = unix.Munmap(m.all)
		m.all = nil
	}
}
