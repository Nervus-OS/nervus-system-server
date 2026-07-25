module github.com/nervus-os/nervus-system-server

go 1.24.0

require (
	github.com/klauspost/compress v1.19.1
	github.com/nervus-os/nervus-ipc/go v0.0.0-20260724232114-a159becba629
)

require google.golang.org/protobuf v1.36.11 // indirect

replace github.com/nervus-os/nervus-ipc/go => ../nervus-ipc/go
