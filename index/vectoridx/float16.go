package vectoridx

import "github.com/evanoberholster/imageIdx/index/vectoridx/internal/kernels"

// Float16ToFloat32 converts one IEEE 754 binary16 value to float32.
func Float16ToFloat32(h uint16) float32 { return kernels.Float16ToFloat32(h) }

// Float32ToFloat16 converts one float32 value to IEEE 754 binary16 using
// round-to-nearest-even. Values outside the finite half range become infinity;
// NaN becomes a quiet NaN.
func Float32ToFloat16(f float32) uint16 { return kernels.Float32ToFloat16(f) }
