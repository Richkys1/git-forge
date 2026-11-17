// Git Forge — vendored static Go core.
//
// The entrypoint (main.ts) spawns this binary and reads its port from stdout:
// the first line is always "[forge] listening on port N". Everything else the
// binary prints is a log line.
//
// Build (byte-reproducible; see BUILD.md):
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o ../forge .
package main

import (
	"fmt"
	"net"
	"os"
)

func main() {
	srv := newServer()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	addr := ln.Addr().(*net.TCPAddr)
	fmt.Printf("[forge] listening on port %d\n", addr.Port)
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[forge] accept: %v\n", err)
			return
		}
		go srv.serveConn(conn)
	}
}
