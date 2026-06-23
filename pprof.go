package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// pprofPreferredAddr is the well-known loopback address tried first. Loopback
// only (127.0.0.1) so the endpoint is never exposed to the network.
const pprofPreferredAddr = "127.0.0.1:8158"

// pprofEndpoint is the per-process discovery record. Concurrent runs each bind
// their own (possibly ephemeral) port, so the actual address is written to a
// per-PID file that `dup-detector pprof-list` reads back.
type pprofEndpoint struct {
	PID     int    `json:"pid"`
	Addr    string `json:"addr"` // host:port actually bound
	Cmd     string `json:"cmd"`
	Started string `json:"started"`
}

// pprofDir holds the per-PID discovery files, next to the MD5 cache (a real,
// user-owned dir), falling back to a temp dir.
func pprofDir() string {
	base := filepath.Dir(DefaultCachePath())
	if base == "" || base == "." {
		base = os.TempDir()
	}
	return filepath.Join(base, "pprof")
}

// startPprof launches net/http/pprof in the background and records a discovery
// file so multiple concurrent runs can be located. Returns a cleanup func to
// defer on clean exit (crashed runs are reaped by the startup sweep instead).
//
// Addressing: DUP_DETECTOR_PPROF="off" disables; an explicit value binds that
// exact address; unset tries 127.0.0.1:8158 first (predictable for a lone run)
// and falls back to an OS-assigned free port (127.0.0.1:0) when 8158 is taken,
// so any number of runs each get their own endpoint. Discover them with
// `dup-detector pprof-list`. Profiling is non-essential: any failure here only
// disables it, never aborts the scan.
func startPprof(status func(string, ...any)) func() {
	noop := func() {}
	sweepStalePprof() // reap discovery files of dead PIDs first

	env := os.Getenv("DUP_DETECTOR_PPROF")
	if env == "off" || env == "0" || env == "false" {
		return noop
	}

	var ln net.Listener
	var err error
	if env != "" {
		ln, err = net.Listen("tcp", env) // explicit address: bind exactly that
	} else {
		ln, err = net.Listen("tcp", pprofPreferredAddr)
		if err != nil {
			ln, err = net.Listen("tcp", "127.0.0.1:0") // 8158 busy → ephemeral
		}
	}
	if err != nil {
		status("warning: pprof disabled (cannot listen: %v)\n", err)
		return noop
	}

	addr := ln.Addr().String()
	status("pprof live profiling on http://%s/debug/pprof/  (list all: dup-detector pprof-list)\n", addr)

	file := writePprofEndpoint(addr)
	go func() {
		if err := http.Serve(ln, nil); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pprof server stopped: %v\n", err)
		}
	}()
	return func() {
		if file != "" {
			_ = os.Remove(file)
		}
	}
}

func writePprofEndpoint(addr string) string {
	dir := pprofDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	chownToInvoker(dir) // hand back to the sudo-invoking user, like the cache
	data, err := json.MarshalIndent(pprofEndpoint{
		PID:     os.Getpid(),
		Addr:    addr,
		Cmd:     strings.Join(os.Args, " "),
		Started: time.Now().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return ""
	}
	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return ""
	}
	chownToInvoker(path)
	return path
}

// sweepStalePprof removes discovery files whose PID is no longer alive.
func sweepStalePprof() {
	dir := pprofDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		if !pidAlive(pid) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// listPprofEndpoints prints the live pprof endpoints (one per running scan).
func listPprofEndpoints(out io.Writer) {
	dir := pprofDir()
	entries, _ := os.ReadDir(dir)
	var n int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var ep pprofEndpoint
		if json.Unmarshal(data, &ep) != nil || !pidAlive(ep.PID) {
			continue
		}
		n++
		fmt.Fprintf(out, "pid=%-8d http://%s/debug/pprof/   started=%s\n    cmd: %s\n",
			ep.PID, ep.Addr, ep.Started, ep.Cmd)
	}
	if n == 0 {
		fmt.Fprintln(out, "No live dup-detector pprof endpoints.")
	}
}

// pidAlive reports whether the process is still running. EPERM means it exists
// but is owned by another user (e.g. a sudo run inspected as the normal user).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid) // always succeeds on Unix
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
