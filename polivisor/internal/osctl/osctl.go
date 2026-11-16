// Package osctl wraps the OS primitives the polivisor needs: per-service
// containment with a hard memory cap, memory sampling and free-memory checks.
package osctl

// Unit contains one service's whole process tree (including forked workers).
type Unit interface {
	Kill() error      // terminate every process in the unit
	Interrupt() error // ask the root process to stop gracefully
	MemBytes() uint64 // current committed memory of the unit
	Close()
}
