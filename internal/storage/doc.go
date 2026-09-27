// Package storage contains the low-level persistence and concurrency
// mechanisms shared by the index implementations: atomic file publication,
// mmap views, directory locks, reader lifetime tracking, framed WAL
// transport, fixed-size manifests, and generation cleanup.
//
// It deliberately contains no domain types or on-disk record policy: magic
// numbers, versions, field layouts, and corruption errors stay in the
// owning index package. Errors that cross the boundary are supplied by the
// caller: View translates mapping failures to the caller's corruption
// sentinels, and MapFile takes the error an empty file should report.
//
// Dependencies point one way: index packages may import storage; storage
// imports only the standard library and golang.org/x/sys.
package storage
