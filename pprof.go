package main

import (
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"os"
)

// pprofDefaultAddr is the loopback address the live profiling/debug HTTP server
// listens on. It is loopback-only (127.0.0.1) so the endpoint is never exposed
// to the network — only processes on this host can reach it.
const pprofDefaultAddr = "127.0.0.1:8158"

// startPprof launches the net/http/pprof server in the background so a live,
// long-running scan can be profiled while it runs (a multi-hour run over
// millions of files cannot be profiled after the fact). It is ON by default:
//
//	go tool pprof http://127.0.0.1:8158/debug/pprof/profile?seconds=30   # CPU
//	go tool pprof http://127.0.0.1:8158/debug/pprof/heap                 # RAM
//	curl  http://127.0.0.1:8158/debug/pprof/goroutine?debug=2            # stacks
//
// The address can be overridden with DUP_DETECTOR_PPROF (e.g. ":6060" or
// "127.0.0.1:9000"); set it to "off" to disable entirely. Binding is
// best-effort: if the port is already taken (a concurrent dup-detector run
// owns it), we warn and proceed without profiling rather than aborting the
// scan. status is the caller's --quiet-aware logger.
func startPprof(status func(string, ...any)) {
	addr := os.Getenv("DUP_DETECTOR_PPROF")
	if addr == "" {
		addr = pprofDefaultAddr
	}
	if addr == "off" || addr == "0" || addr == "false" {
		return
	}

	// Bind synchronously so a port conflict is reported as a warning here,
	// before we serve in the background.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		status("warning: pprof disabled (cannot listen on %s: %v)\n", addr, err)
		return
	}
	status("pprof live profiling on http://%s/debug/pprof/\n", addr)
	go func() {
		// http.Serve only returns on a fatal error; profiling is non-essential
		// so a failure here must never take down the scan.
		if err := http.Serve(ln, nil); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pprof server stopped: %v\n", err)
		}
	}()
}
