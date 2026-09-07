package idset

import (
	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/roar"
)

// Builder is a writer-owned mutable copy used to construct a replacement Ref.
// Freeze transfers its current bitmap to an immutable Ref; callers must not
// mutate the Builder afterward.
type Builder struct {
	mutable *roar.DeltaBuilder
}

func Clone(ref Ref) Builder {
	return Builder{mutable: roar.NewDeltaBuilderFrom(ref.bitmap)}
}

func (builder *Builder) AddMany(ids []imageidx.ImageID) {
	if len(ids) != 0 {
		builder.mutable.AddMany(ids)
	}
}

func (builder *Builder) Remove(id imageidx.ImageID) {
	builder.mutable.Remove(id)
}

func (builder *Builder) Or(ref Ref) {
	if ref.Valid() {
		builder.mutable.Or(ref.bitmap)
	}
}

func (builder *Builder) And(ref Ref) {
	if !ref.Valid() {
		builder.mutable = roar.NewDeltaBuilder()
		return
	}
	builder.mutable.And(ref.bitmap)
}

func (builder *Builder) AndNot(ref Ref) {
	if ref.Valid() {
		builder.mutable.AndNot(ref.bitmap)
	}
}

// ApplyDelta folds additions minus removals into the builder in a single
// pass. Invalid refs are treated as empty.
func (builder *Builder) ApplyDelta(add, remove Ref) {
	var addBm, removeBm roar.Bitmap
	if add.Valid() {
		addBm = add.bitmap
	}
	if remove.Valid() {
		removeBm = remove.bitmap
	}
	builder.mutable.ApplyDelta(addBm, removeBm)
}

func (builder *Builder) Freeze() Ref {
	if builder == nil || builder.mutable == nil {
		return Empty()
	}
	bitmap := builder.mutable.Freeze()
	builder.mutable = nil
	return Ref{bitmap: bitmap}
}

// Operands collects immutable refs for roar's optimized multi-set kernels
// without exposing concrete bitmap values to sibling packages.
type Operands struct {
	refs []roar.Bitmap
}

func NewOperands(capacity int) Operands {
	return Operands{refs: make([]roar.Bitmap, 0, capacity)}
}

func (operands *Operands) Add(ref Ref) {
	if !ref.Valid() {
		return
	}
	operands.refs = append(operands.refs, ref.bitmap)
}

func (operands Operands) And() Ref {
	switch len(operands.refs) {
	case 0:
		return Empty()
	case 1:
		return Ref{bitmap: operands.refs[0]}
	default:
		return Ref{bitmap: roar.AndMany(operands.refs...)}
	}
}

func (operands Operands) Or() Ref {
	switch len(operands.refs) {
	case 0:
		return Empty()
	case 1:
		return Ref{bitmap: operands.refs[0]}
	default:
		return Ref{bitmap: roar.FastParOr(1, operands.refs...)}
	}
}
