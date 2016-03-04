package main

import (
	"fmt"
	"os/exec"
	"time"
)

// interact with dockerd to detect docker errors
// try `docker ps` 5 times
// if it returns normally once, consider the system healthy
// if it hangs for >30s every time, consider the system unhealthy
func (m *Monitor) Docker() {
	m.logSystemMetric("docker at=start", "", true)

	for _ = range time.Tick(MONITOR_INTERVAL) {
		var err error
		unhealthy := true

		for i := 0; i < 5; i++ {
			fmt.Printf("docker try=%d\n", i)

			cmd := exec.Command("docker", "ps")

			if err := cmd.Start(); err != nil {
				m.logSystemMetric("docker at=error", fmt.Sprintf("count#DockerCommandStart.error=1 err=%q", err), true)
				continue
			}

			timer := time.AfterFunc(30*time.Second, func() {
				cmd.Process.Kill()
			})

			err = cmd.Wait()
			timer.Stop()

			// docker ps command returned 0
			if err == nil {
				unhealthy = false
				break
			}
		}

		// docker ps never ran without error
		if unhealthy {
			m.SetUnhealthy("docker", err)
		} else {
			m.logSystemMetric("docker at=ok", "", true)
		}
	}
}
