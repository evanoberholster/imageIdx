package vectoridx

import "time"

// Option configures runtime policy for Create and Open. Options are not part
// of the persisted format and are not validated against the stored index.
type Option func(*options)

type options struct {
	syncInterval    time.Duration
	walSegmentBytes int64
}

// WithSyncInterval enables periodic WAL synchronization when durability is
// DurabilityOS: mutations are fsynced when the async queue drains and at
// least every d, so a batch of writes shares one fsync. Close always syncs
// and Sync establishes an explicit durable boundary at any time. Zero (the
// default) leaves records in the OS page cache; negative is rejected by
// Create/Open with ErrInvalidConfig.
//
// WithSyncInterval is ignored for DurabilitySync, which already fsyncs every
// mutation.
func WithSyncInterval(d time.Duration) Option {
	return func(o *options) { o.syncInterval = d }
}

// WithWALSegmentBytes sets the WAL rotation threshold. A checkpoint removes
// every segment except the active one, so the threshold bounds single-file
// growth between checkpoints. Zero (the default) uses
// DefaultWALSegmentBytes; negative is rejected by Create/Open.
func WithWALSegmentBytes(n int64) Option {
	return func(o *options) { o.walSegmentBytes = n }
}

func applyOptions(opts []Option) (options, error) {
	o := options{walSegmentBytes: DefaultWALSegmentBytes}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.syncInterval < 0 || o.walSegmentBytes < 1 {
		return options{}, ErrInvalidConfig
	}
	return o, nil
}
