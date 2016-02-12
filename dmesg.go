package main

import (
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// grep dmesg for file system error strings
// if grep exits 0 it was a match so we mark the instance unhealthy
// if grep exits 1 there was no match so we carry on
func (m *Monitor) Dmesg() {
	m.logSystemMetric("dmesg at=start", "", true)

	for _ = range time.Tick(MONITOR_INTERVAL) {
		m.grep("Remounting filesystem read-only")
		m.grep("switching pool to read-only mode")
	}
}

func (m *Monitor) grep(pattern string) {
	cmd := exec.Command("sh", "-c", fmt.Sprintf("dmesg | grep %q", pattern))
	out, err := cmd.CombinedOutput()

	// grep returned 0
	if err == nil {
		m.SetUnhealthy("dmesg", fmt.Errorf("dmesg reported %q", out))
	} else {
		m.logSystemMetric("dmesg at=ok", "", true)
	}
}

// Dump dmesg to convox log stream and rollbar
func (m *Monitor) ReportDmesg() {
	out, err := exec.Command("dmesg").CombinedOutput()

	if err != nil {
		m.ReportError(err)
	} else {
		m.ReportError(errors.New(string(out)))
	}
}
