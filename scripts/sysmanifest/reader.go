package main

import (
	"bytes"
	"io"
)

// newReader 是 bytes.NewReader 的薄封装，仅为让 main.go 的解析处读起来更直白。
func newReader(b []byte) io.Reader { return bytes.NewReader(b) }
