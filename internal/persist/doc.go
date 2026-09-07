// Package persist contains file-backed persistence mechanics shared by the
// index implementations: mmap views, framed WAL transport, fixed-size
// manifests, and generation cleanup.
//
// It deliberately contains no domain types or on-disk record policy: magic
// numbers, versions, field layouts, and corruption errors stay in the owning
// index package. Dependencies point one way: index packages may import
// persist; persist imports only internal/indexutil and the standard library.
package persist
