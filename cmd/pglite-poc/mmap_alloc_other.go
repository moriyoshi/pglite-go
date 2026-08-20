//go:build wazero && !darwin && !linux

package main

import "context"

// withMemoryAllocator is a no-op on platforms without the mmap allocator; wazero
// falls back to its default (copy-on-grow) linear memory.
func withMemoryAllocator(ctx context.Context) context.Context { return ctx }
