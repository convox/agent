package main

import (
	"os/exec"
	"time"
)

// interact with dockerd to detect docker errors
// try `docker ps` 5 times
// if it returns normally once, consider the system healthy
// if it hangs for >30s every time, consider the system unhealthy
func (m *Monitor) Docker() {
	m.logSystemf("docker at=start")

	for _ = range time.Tick(MONITOR_INTERVAL) {
		var err error
		unhealthy := true

		for i := 0; i < 5; i++ {
			m.logSystemf("docker exec.Command args=ps try=%d", i)

			cmd := exec.Command("docker", "ps")

			if err := cmd.Start(); err != nil {
				m.logSystemf("docker exec.Command args=ps try=%d count#DockerPsError=1 err=%q", i, err)
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
			m.logSystemf("docker ok=true")
		}
	}
}
