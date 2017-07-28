package main

import "time"

var MONITOR_INTERVAL = 5 * time.Minute

func main() {
	monitor := NewMonitor()

	go monitor.Containers()
	go monitor.Disk()
	go monitor.Docker()
	go monitor.Dmesg()
	go monitor.Spot()

	for {
		time.Sleep(60 * time.Second)
	}
}
