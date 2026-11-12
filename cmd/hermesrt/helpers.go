package main

import (
	"bytes"
	"strconv"
)

func itoa(n int) string { return strconv.Itoa(n) }

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
