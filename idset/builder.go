package idset

import (
	"github.com/dgraph-io/sroar"
	imageidx "github.com/evanoberholster/imageIdx"
)

// Builder is a writer-owned mutable copy used to construct a replacement Ref.
// Freeze transfers its current bitmap to an immutable Ref; callers must not
// mutate the Builder afterward.
type Builder struct {
	bitmap *sroar.Bitmap
}

func Clone(ref Ref) Builder {
	if ref.bitmap == nil {
		return Builder{bitmap: sroar.NewBitmap()}
	}
	return Builder{bitmap: ref.bitmap.Clone()}
}

func (builder *Builder) AddMany(ids []imageidx.ImageID) {
	if len(ids) != 0 {
		builder.bitmap.SetMany(ids)
	}
}

func (builder *Builder) Remove(id imageidx.ImageID) {
	builder.bitmap.Remove(id)
}

func (builder *Builder) Or(ref Ref) {
	if ref.bitmap != nil {
		builder.bitmap.Or(ref.bitmap)
	}
}

func (builder *Builder) And(ref Ref) {
	if ref.bitmap == nil {
		builder.bitmap = sroar.NewBitmap()
		return
	}
	builder.bitmap.And(ref.bitmap)
}

func (builder *Builder) AndNot(ref Ref) {
	if ref.bitmap != nil {
		builder.bitmap.AndNot(ref.bitmap)
	}
}

func (builder *Builder) Cleanup() {
	builder.bitmap.Cleanup()
}

func (builder *Builder) Freeze() Ref {
	if builder == nil || builder.bitmap == nil {
		return Empty()
	}
	bitmap := builder.bitmap
	builder.bitmap = nil
	return Ref{bitmap: bitmap}
}

// Operands collects immutable refs for sroar's optimized multi-set kernels
// without exposing concrete bitmap pointers to sibling packages.
type Operands struct {
	refs   []*sroar.Bitmap
	single Ref
}

func NewOperands(capacity int) Operands {
	return Operands{refs: make([]*sroar.Bitmap, 0, capacity)}
}

func (operands *Operands) Add(ref Ref) {
	if ref.bitmap == nil {
		return
	}
	if len(operands.refs) == 0 {
		operands.single = ref
	}
	operands.refs = append(operands.refs, ref.bitmap)
}

func (operands Operands) And() Ref {
	switch len(operands.refs) {
	case 0:
		return Empty()
	case 1:
		return operands.single
	default:
		// sroar.FastAnd consumes its first argument. Clone it so Operands never
		// violates the immutability of an added Ref.
		owned := append([]*sroar.Bitmap(nil), operands.refs...)
		owned[0] = owned[0].Clone()
		return Ref{bitmap: sroar.FastAnd(owned...)}
	}
}

func (operands Operands) Or() Ref {
	switch len(operands.refs) {
	case 0:
		return Empty()
	case 1:
		return operands.single
	default:
		return Ref{bitmap: sroar.FastOr(operands.refs...)}
	}
}
