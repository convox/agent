package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os/exec"
	"time"

	"github.com/convox/agent/Godeps/_workspace/src/github.com/aws/aws-sdk-go/aws"
	"github.com/convox/agent/Godeps/_workspace/src/github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/convox/agent/Godeps/_workspace/src/github.com/aws/aws-sdk-go/service/ecs"
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
		m.EvictInstance("dmesg", string(out))
	} else {
		m.logSystemMetric("dmesg at=ok", "", true)
	}
}

func (m *Monitor) EvictInstance(reason, out string) {
	m.logSystemMetric(reason, fmt.Sprintf("at=error count#AutoScaling.SetInstanceHealth=1 out=%q", out), true)

	m.ReportDmesg()

	AutoScaling := autoscaling.New(&aws.Config{})

	_, err := AutoScaling.SetInstanceHealth(&autoscaling.SetInstanceHealthInput{
		HealthStatus:             aws.String("Unhealthy"),
		InstanceId:               aws.String(m.instanceId),
		ShouldRespectGracePeriod: aws.Bool(true),
	})

	if err != nil {
		m.logSystemMetric(reason, fmt.Sprintf("at=error count#AutoScaling.SetInstanceHealth.error=1 err=%q", err), true)
	}

	em, err := m.GetECSMetadata()

	if err != nil {
		m.logSystemMetric(reason, fmt.Sprintf("at=error count#GetECSMetadata.error=1 err=%q", err), true)
		return
	}

	ECS := ecs.New(&aws.Config{})

	_, err = ECS.DeregisterContainerInstance(&ecs.DeregisterContainerInstanceInput{
		Cluster:           aws.String(em.Cluster),
		ContainerInstance: aws.String(em.ContainerInstanceArn),
	})

	if err != nil {
		m.logSystemMetric(reason, fmt.Sprintf("at=error count#ECS.DeregisterContainerInstance.error=1 err=%q", err), true)
	} else {
		m.logSystemMetric(reason, fmt.Sprintf("at=ok count#ECS.DeregisterContainerInstance=1 err=%q", err), true)
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

// http://docs.aws.amazon.com/AmazonECS/latest/developerguide/ecs-agent-introspection.html

type ECSMetadata struct {
	Cluster              string
	ContainerInstanceArn string
	Version              string
}

// http://docs.aws.amazon.com/AmazonECS/latest/developerguide/ecs-agent-introspection.html
func (m *Monitor) GetECSMetadata() (*ECSMetadata, error) {
	res, err := http.Get("http://localhost:51678/v1/metadata")

	if err != nil {
		return nil, err
	}

	body, err := ioutil.ReadAll(res.Body)
	res.Body.Close()

	em := ECSMetadata{}

	err = json.Unmarshal(body, &em)

	return &em, err
}
