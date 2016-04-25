package main

import (
	"fmt"
	"os/exec"
	"time"
)

// grep dmesg for file system error strings
// if grep exits 0 it was a match so we mark the instance unhealthy
// if grep exits 1 there was no match so we carry on
func (m *Monitor) Dmesg() {
	m.logSystemf("dmesg at=start")

	for _ = range time.Tick(MONITOR_INTERVAL) {
		m.grep("Remounting filesystem read-only")
		m.grep("switching pool to read-only mode")
	}
}

func (m *Monitor) grep(pattern string) {
	m.logSystemf("dmesg grep pattern=%q at=start", pattern)

	cmd := exec.Command("sh", "-c", fmt.Sprintf("dmesg | grep %q", pattern))
	out, err := cmd.CombinedOutput()

	// grep returned 0
	if err == nil {
		m.SetUnhealthy("dmesg", fmt.Errorf(string(out)))
	} else {
		m.logSystemf("dmesg ok=true")
	}
}
