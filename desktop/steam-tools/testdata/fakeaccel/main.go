// fakeaccel stands in for tetherflow-steam-accelerator.exe in the launcher test
// (steam-tools/Test-Launcher.ps1). It records what the launcher gave it and exits with a
// chosen code. It never opens a network connection, changes hosts or creates a file other
// than its record.
//
// Environment:
//
//	FAKEACCEL_RECORD      file that receives one JSON line per invocation (required)
//	FAKEACCEL_DOCTOR_EXIT exit code of a --doctor call (default 0)
//	FAKEACCEL_EXIT        exit code of every other call (default 0)
//	FAKEACCEL_READ_STDIN  1: read standard input (at most 64 KiB) for a non-doctor call
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
)

type invocation struct {
	Args       []string `json:"args"`
	Doctor     bool     `json:"doctor"`
	StdinRead  bool     `json:"stdin_read"`
	StdinBytes int      `json:"stdin_bytes"`
	StdinText  string   `json:"stdin_text"`
}

func exitCode(name string) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

func main() {
	record := os.Getenv("FAKEACCEL_RECORD")
	if record == "" {
		fmt.Fprintln(os.Stderr, "fakeaccel: FAKEACCEL_RECORD is not set")
		os.Exit(90)
	}
	inv := invocation{Args: os.Args[1:]}
	for _, a := range inv.Args {
		if a == "--doctor" {
			inv.Doctor = true
		}
	}
	if !inv.Doctor && os.Getenv("FAKEACCEL_READ_STDIN") == "1" {
		data, _ := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
		inv.StdinRead, inv.StdinBytes, inv.StdinText = true, len(data), string(data)
	}
	line, err := json.Marshal(inv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeaccel:", err)
		os.Exit(91)
	}
	f, err := os.OpenFile(record, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeaccel:", err)
		os.Exit(92)
	}
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeaccel:", err)
		os.Exit(93)
	}
	code := exitCode("FAKEACCEL_EXIT")
	if inv.Doctor {
		code = exitCode("FAKEACCEL_DOCTOR_EXIT")
	}
	fmt.Printf("fakeaccel: recorded %d argument(s), exiting %d\n", len(inv.Args), code)
	os.Exit(code)
}
