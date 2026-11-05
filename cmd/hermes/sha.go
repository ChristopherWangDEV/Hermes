package main

import "crypto/sha256"

// sha256Of 是给 verify 打印用的短摘要（完整值由 sealed 的 manifest 承载）。
func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
