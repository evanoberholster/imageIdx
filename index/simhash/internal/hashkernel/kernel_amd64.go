//go:build amd64

package hashkernel

// On amd64 math/bits.OnesCount64 lowers to the POPCNT instruction, so the
// scalar reference already uses the hardware population count. Current is left
// as Scalar. A vector backend (AVX2 Harley-Seal, or AVX-512 VPOPCNTDQ) can be
// installed here behind runtime CPU feature detection; it must be tested
// against Scalar before use.
