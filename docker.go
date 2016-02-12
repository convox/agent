package main

import (
	"fmt"
	"os/exec"
	"time"
)

// interact with dockerd for docker errors
// if `docker ps` exits non-zero we mark the instance unhealthy
func (m *Monitor) Docker() {
	m.logSystemMetric("docker at=start", "", true)

	for _ = range time.Tick(MONITOR_INTERVAL) {
		cmd := exec.Command("docker", "ps")

		if err := cmd.Start(); err != nil {
			m.logSystemMetric("docker at=error", fmt.Sprintf("count#Command.Start.error=1 err=%q", err), true)
			continue
		}

		timer := time.AfterFunc(10*time.Second, func() {
			cmd.Process.Kill()
		})

		err := cmd.Wait()
		timer.Stop()

		// docker ps returned non-zero
		if err != nil {
			m.SetUnhealthy("docker", err)
		} else {
			m.logSystemMetric("docker at=ok", "", true)
		}
	}
}
