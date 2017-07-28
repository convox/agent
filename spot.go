package main

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
)

func (m *Monitor) Spot() {
	m.logSystemf("spot at=start")

	cfg := ec2metadata.Config{}

	if os.Getenv("EC2_METADATA_ENDPOINT") != "" {
		cfg.Endpoint = aws.String(os.Getenv("EC2_METADATA_ENDPOINT"))
	}

	svc := ec2metadata.New(&cfg)

	for _ = range time.Tick(5 * time.Second) {
		if os.Getenv("DEVELOPMENT") != "true" && svc.Available() {
			tt, err := svc.GetMetadata("spot/termination-time")
			if err != nil {
				m.logSystemf("Unable to fetch termination time")
			} else {
				ts, err := time.Parse(time.RFC3339, tt)
				if err != nil {
					m.logSystemf("Unable to parse termination time")
				} else {
					m.logSystemf("Termination notice: %s", ts)
					instanceArn := m.getECSMetadata("ContainerInstanceArn")
					cluster := m.getECSMetadata("Cluster")
					m.setInstanceDraining(instanceArn, cluster)
				}
			}
		}
	}
}

func (m *Monitor) getECSMetadata(key string) string {
	resp, err := http.Get("http://localhost:51678/v1/metadata")
	if err != nil {
		m.logSystemf("Unable to fetch instance ARN")
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		m.logSystemf("Unable to read metadata response body")
	}

	var metadata map[string]interface{}

	if err = json.Unmarshal(body, &metadata); err != nil {
		m.logSystemf("Unable to decode JSON metadata")
	}

	return metadata[key].(string)
}

func (m *Monitor) setInstanceDraining(instanceArn, cluster string) {
	err := exec.Command("aws", "ecs", "update-container-instances-state", "--cluster", cluster, "--container-instances", instanceArn, "--status", "DRAINING").Run()
	if err != nil {
		m.logSystemf("Unable to set EC2 instance state to DRAINING for %s", instanceArn)
	}
}
