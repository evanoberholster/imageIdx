package schema

import "errors"

var (
	ErrCorrupt         = errors.New("schema: corrupt allocation file")
	ErrClosed          = errors.New("schema: closed")
	ErrNamespace       = errors.New("schema: invalid namespace")
	ErrNamespaceExists = errors.New("schema: duplicate namespace")
	ErrExhausted       = errors.New("schema: ID exhausted")
	ErrLocked          = errors.New("schema: allocation file locked")
	ErrVersion         = errors.New("schema: unsupported format version")
	ErrCount           = errors.New("schema: allocation count must be positive")
)
