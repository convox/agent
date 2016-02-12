package main

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/convox/agent/Godeps/_workspace/src/github.com/docker/go-units"
)

// Monitor Disk Metrics for Instance
// Currently this only accurrately reports disk usage on the Amazon ECS AMI and the devicemapper driver
// not Docker Machine, boot2docker and aufs driver
func (m *Monitor) Disk() {
	m.logSystemMetric("disk at=start", "", true)

	for _ = range time.Tick(MONITOR_INTERVAL) {
		// Report Docker utilization
		a, t, u, docker_util, err := m.DockerUtilization()

		if err != nil {
			m.logSystemMetric("disk at=error", fmt.Sprintf("err=%q", err), true)
		} else {
			m.logSystemMetric("disk", fmt.Sprintf("dim#volume=docker dim#instanceId=%s sample#disk.available=%.4fgB sample#disk.total=%.4fgB sample#disk.used=%.4fgB sample#disk.utilization=%.2f%%", m.instanceId, a, t, u, docker_util), true)
		}

		// If disk is over 80.0 full, delete docker containers and images in attempt to reclaim space
		if docker_util > 80.0 {
			m.RemoveDockerArtifacts()
		}

		// Report root volume utilization after artifacts have possibly been removed
		path := "/mnt/host_root"
		a, t, u, root_util, err := m.PathUtilization(path)

		if err != nil {
			m.logSystemMetric("disk at=error", fmt.Sprintf("path=%s err=%q", path, err), true)
		} else {
			m.logSystemMetric("disk", fmt.Sprintf("dim#volume=root dim#instanceId=%s sample#disk.available=%.4fgB sample#disk.total=%.4fgB sample#disk.used=%.4fgB sample#disk.utilization=%.2f%%", m.instanceId, a, t, u, root_util), true)
		}

		if root_util >= 99.9 {
			m.SetUnhealthy("disk", fmt.Errorf("root volume is %.2f%% full", root_util))
		}
	}
}

func (m *Monitor) DockerUtilization() (avail, total, used, util float64, err error) {
	info, err := m.client.Info()

	if err != nil {
		return
	}

	status := [][]string{}

	err = info.GetJSON("DriverStatus", &status)

	if err != nil {
		return
	}

	var a, t, u int64

	for _, v := range status {
		if v[0] == "Data Space Available" {
			a, err = units.FromHumanSize(v[1])

			if err != nil {
				return
			}
		}

		if v[0] == "Data Space Total" {
			t, err = units.FromHumanSize(v[1])

			if err != nil {
				return
			}
		}

		if v[0] == "Data Space Used" {
			u, err = units.FromHumanSize(v[1])

			if err != nil {
				return
			}
		}
	}

	if t == 0 {
		err = fmt.Errorf("no docker volume information for %s driver", m.dockerDriver)
		return
	}

	avail = float64(a) / 1000 / 1000 / 1000
	total = float64(t) / 1000 / 1000 / 1000
	used = float64(u) / 1000 / 1000 / 1000
	util = used / total * 100

	return
}

func (m *Monitor) PathUtilization(path string) (avail, total, used, util float64, err error) {
	// https://github.com/StalkR/goircbot/blob/master/lib/disk/space_unix.go
	s := syscall.Statfs_t{}
	err = syscall.Statfs(path, &s)

	if err != nil {
		return
	}

	t := int(s.Bsize) * int(s.Blocks)
	f := int(s.Bsize) * int(s.Bfree)

	total = (float64)(t) / 1024 / 1024 / 1024
	avail = (float64)(f) / 1024 / 1024 / 1024
	used = (float64)(t-f) / 1024 / 1024 / 1024
	util = used / (used + avail) * 100

	return
}

// Force remove docker containers, volumes and images
// This is a quick and dirty way to remove everything but running containers their images
// This will blow away build or run cache but hopefully preserve
// disk space.
func (m *Monitor) RemoveDockerArtifacts() {
	m.logSystemMetric("disk", "count#docker.rmi=1", true)

	m.run(`docker rm -v $(docker ps -a -q)`)
	m.run(`docker rmi -f $(docker images -a -q)`)
}

// Blindly run a shell command and log its output and error
func (m *Monitor) run(cmd string) {
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()

	lines := strings.Split(string(out), "\n")

	for _, l := range lines {
		m.logSystemMetric("disk run", fmt.Sprintf("cmd=%q out=%q", cmd, l), true)
	}

	if err != nil {
		m.logSystemMetric("disk run", fmt.Sprintf("error=%q", err), true)
	}
}
