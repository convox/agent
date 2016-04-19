package main

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/awslogs"
	docker "github.com/fsouza/go-dockerclient"
)

func (m *Monitor) Containers() {
	m.handleRunning()
	m.handleExited()

	ch := make(chan *docker.APIEvents)

	go m.handleEvents(ch)
	go m.streamLogs()

	m.client.AddEventListener(ch)
}

// List already running containers and subscribe and stream logs
func (m *Monitor) handleRunning() {
	containers, err := m.client.ListContainers(docker.ListContainersOptions{})

	if err != nil {
		log.Fatal(err)
	}

	for _, container := range containers {
		shortId := container.ID[0:12]

		// Don't subscribe and stream logs from the agent container itself
		img := container.Image

		if strings.HasPrefix(img, "convox/agent") || strings.HasPrefix(img, "agent/agent") {
			m.agentId = container.ID
			m.agentImage = img

			parts := strings.SplitN(img, ":", 2)
			if len(parts) == 2 {
				m.agentVersion = parts[1]
			}
		}

		fmt.Printf("monitor event id=%s status=started\n", shortId)
		m.handleCreate(container.ID)
	}
}

// List already exiteded containers and remove
func (m *Monitor) handleExited() {
	containers, err := m.client.ListContainers(docker.ListContainersOptions{
		Filters: map[string][]string{
			"status": []string{"exited"},
		},
	})

	if err != nil {
		log.Fatal(err)
	}

	for _, container := range containers {
		shortId := container.ID[0:12]

		fmt.Printf("monitor event id=%s status=died\n", shortId)
		m.handleDie(container.ID)
	}
}

func (m *Monitor) handleEvents(ch chan *docker.APIEvents) {
	for event := range ch {

		shortId := event.ID

		if len(shortId) > 12 {
			shortId = shortId[0:12]
		}

		switch event.Status {
		case "create":
			m.handleCreate(event.ID)
		case "die":
			m.handleDie(event.ID)
		case "kill":
			m.handleKill(event.ID)
		case "oom":
			m.handleOom(event.ID)
		case "start":
			m.handleStart(event.ID)
		case "stop":
			m.handleStop(event.ID)
		}

		metric := "DockerEvent" + ucfirst(event.Status)
		msg := fmt.Sprintf("id=%s time=%d count#%s=1", event.ID, event.Time, metric)

		if env, ok := m.envs[event.ID]; ok {
			if p := env["PROCESS"]; p != "" {
				msg = fmt.Sprintf("id=%s process=%s time=%d count#%s=1", event.ID, p, event.Time, metric)
			}
		}

		m.logSystemMetric("container handleEvents", msg, true)
	}
}

// Inspect created or existing container
// Extract env and create awslogger, and save on monitor struct
func (m *Monitor) handleCreate(id string) {
	container, env, err := m.inspectContainer(id)

	if err != nil {
		m.logSystemMetric("container handleCreate at=error", fmt.Sprintf("count#DockerInspectError=1 err=%q", err), true)
		return
	}

	m.envs[id] = env

	// create a an awslogger and associated CloudWatch Logs LogGroup
	if env["LOG_GROUP"] != "" {
		awslogger, aerr := m.StartAWSLogger(container, env["LOG_GROUP"])

		if aerr != nil {
			m.logSystemMetric("container StartAWSLogger at=error", fmt.Sprintf("id=%s logGroup=%s process=%s err=%q", id, env["LOG_GROUP"], env["PROCESS"], aerr), true)
		} else {
			m.logSystemMetric("container StartAWSLogger at=ok", fmt.Sprintf("id=%s logGroup=%s process=%s", id, env["LOG_GROUP"], env["PROCESS"]), true)
			m.loggers[id] = awslogger
		}
	}

	msg := fmt.Sprintf("Starting process %s", id[0:12])

	if p := env["PROCESS"]; p != "" {
		msg = fmt.Sprintf("Starting %s process %s", p, id[0:12])
	}

	m.logAppEvent(id, msg)
}

func (m *Monitor) handleDie(id string) {
	// While we could remove a container and volumes on this event
	// It seems like explicitly doing a `docker run --rm` is the best way
	// to state this intent.

	msg := fmt.Sprintf("Docker event for process %s - die", id[0:12])

	if p := m.envs[id]["PROCESS"]; p != "" {
		msg = fmt.Sprintf("Docker event for %s process %s - die", p, id[0:12])
	}

	m.logAppEvent(id, msg)
}

func (m *Monitor) handleKill(id string) {
	msg := fmt.Sprintf("Docker event for process %s - kill", id[0:12])

	if p := m.envs[id]["PROCESS"]; p != "" {
		msg = fmt.Sprintf("Docker event for %s process %s - kill", p, id[0:12])
	}

	m.logAppEvent(id, msg)
}

func (m *Monitor) handleOom(id string) {
	msg := fmt.Sprintf("Docker event for process %s - oom", id[0:12])

	if p := m.envs[id]["PROCESS"]; p != "" {
		msg = fmt.Sprintf("Docker event for %s process %s - oom", p, id[0:12])
	}

	m.logAppEvent(id, msg)
}

func (m *Monitor) handleStart(id string) {
	m.updateCgroups(id)

	if id != m.agentId {
		go m.subscribeLogs(id)
	}
}

func (m *Monitor) handleStop(id string) {
	msg := fmt.Sprintf("Docker event for process %s - stop", id[0:12])

	if p := m.envs[id]["PROCESS"]; p != "" {
		msg = fmt.Sprintf("Docker event for %s process %s - stop", p, id[0:12])
	}

	m.logAppEvent(id, msg)
}

func (m *Monitor) inspectContainer(id string) (*docker.Container, map[string]string, error) {
	env := map[string]string{}

	container, err := m.client.InspectContainer(id)

	if err != nil {
		log.Printf("error: %s\n", err)
		return container, env, err
	}

	for _, e := range container.Config.Env {
		parts := strings.SplitN(e, "=", 2)

		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}

	return container, env, nil
}

// Modify the container cgroup to enable swap if SWAP=1 is set
func (m *Monitor) updateCgroups(id string) {
	env := m.envs[id]

	if env["SWAP"] == "1" {
		// sleep to address observed race for cgroups setup
		// error: open /cgroup/memory/docker/6a3ea224a5e26657207f6c3d3efad072e3a5b02ec3e80a5a064909d9f882e402/memory.memsw.limit_in_bytes: no such file or directory
		time.Sleep(1 * time.Second)

		shortId := id[0:12]

		bytes := "18446744073709551615"

		fmt.Printf("monitor cgroups id=%s cgroup=memory.memsw.limit_in_bytes value=%s\n", shortId, bytes)
		err := ioutil.WriteFile(fmt.Sprintf("/cgroup/memory/docker/%s/memory.memsw.limit_in_bytes", id), []byte(bytes), 0644)

		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
		}

		fmt.Printf("monitor cgroups id=%s cgroup=memory.soft_limit_in_bytes value=%s\n", shortId, bytes)
		err = ioutil.WriteFile(fmt.Sprintf("/cgroup/memory/docker/%s/memory.soft_limit_in_bytes", id), []byte(bytes), 0644)

		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
		}

		fmt.Printf("monitor cgroups id=%s cgroup=memory.limit_in_bytes value=%s\n", shortId, bytes)
		err = ioutil.WriteFile(fmt.Sprintf("/cgroup/memory/docker/%s/memory.limit_in_bytes", id), []byte(bytes), 0644)

		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
		}
	}
}

func (m *Monitor) subscribeLogs(id string) {
	env := m.envs[id]

	kinesis := env["KINESIS"]
	logGroup := env["LOG_GROUP"]
	process := env["PROCESS"]
	release := env["RELEASE"]

	logResource := kinesis
	if logResource == "" {
		logResource = logGroup
	}

	if logResource == "" {
		m.logSystemMetric("container subscribeLogs at=skip", fmt.Sprintf("id=%s kinesis=%s logGroup=%s process=%s", id, kinesis, logGroup, process), true)
		return
	}

	m.logSystemMetric("container subscribeLogs at=start", fmt.Sprintf("id=%s kinesis=%s logGroup=%s process=%s", id, kinesis, logGroup, process), true)

	r, w := io.Pipe()

	go func(prefix string, r io.ReadCloser) {
		defer r.Close()

		m.logSystemMetric("container subscribeLogs.Scan at=start", fmt.Sprintf("prefix=%s", prefix), true)

		scanner := bufio.NewScanner(r)

		for scanner.Scan() {
			text := scanner.Text()
			// The expectation is a single log line from the Docker daemon:
			// 2016-04-14T23:29:35.995734263Z Hello from Docker.

			// split and parse docker timestamp
			ts := time.Now()

			parts := strings.SplitN(text, " ", 2)
			if len(parts) == 2 {
				t, err := time.Parse(time.RFC3339Nano, parts[0])
				if err != nil {
					fmt.Printf("container subscribeLogs.Scan time.Parse err=%q\n", err)
				} else {
					ts = t
					text = parts[1]
				}
			}

			// append syslog-ish prefix:
			// web:RXZMCQEPDKO/1d11a78279e0 Hello from Docker.
			line := fmt.Sprintf("%s:%s/%s %s", process, release, id[0:12], text)

			if kinesis != "" {
				m.addLine(kinesis, []byte(fmt.Sprintf("%s %s", ts.Format("2006-01-02 15:04:05"), line))) // add timestamp to kinesis for legacy purposes
			}

			if awslogger, ok := m.loggers[id]; ok {
				awslogger.Log(&logger.Message{
					ContainerID: id,
					Line:        []byte(line),
					Timestamp:   ts,
				})
			}
		}

		if scanner.Err() != nil {
			m.logSystemMetric("container subscribeLogs.Scan at=error", fmt.Sprintf("count#ScannerError=1 err=%q", scanner.Err().Error()), true)
		}

		m.logSystemMetric("container subscribeLogs.Scan at=return", fmt.Sprintf("prefix=%s", prefix), true)
	}(process, r)

	// tail docker logs and write to pipe
	// start close to Now() so agent restarts dont replay too many logs
	since := time.Now().Add(-15 * time.Second).Unix()

	for {
		m.logSystemMetric("container subscribeLogs", fmt.Sprintf("id=%s since=%d", id, since), true)

		err := m.client.Logs(docker.LogsOptions{
			Since:        since,
			Container:    id,
			Follow:       true,
			Stdout:       true,
			Stderr:       true,
			Tail:         "all",
			Timestamps:   true,
			RawTerminal:  false,
			OutputStream: w,
			ErrorStream:  w,
		})

		since = time.Now().Unix() // update cursor to now in anticipation of retry

		if err != nil {
			m.logSystemMetric("container subscribeLogs", fmt.Sprintf("id=%s count#DockerLogsError=1 err=%q", id, err), true)
		}

		container, err := m.client.InspectContainer(id)

		if err != nil {
			m.logSystemMetric("container subscribeLogs", fmt.Sprintf("id=%s count#DockerInspectContainerError=1 err=%q", id, err), true)
			break
		}

		if container.State.Running == false {
			break
		}
	}

	w.Close()

	if awslogger, ok := m.loggers[id]; ok {
		err := awslogger.Close()

		if err != nil {
			m.logSystemMetric("container awslogger.Close at=error", fmt.Sprintf("id=%s logGroup=%s process=%s err=%q", id, logGroup, process, err), true)
		} else {
			m.logSystemMetric("container awslogger.Close at=ok", fmt.Sprintf("id=%s logGroup=%s process=%s", id, logGroup, process), true)
		}

		delete(m.loggers, id)
	}

	m.logSystemMetric("container subscribeLogs at=return", fmt.Sprintf("id=%s kinesis=%s logGroup=%s process=%s", id, kinesis, logGroup, process), true)
}

func (m *Monitor) StartAWSLogger(container *docker.Container, logGroup string) (logger.Logger, error) {
	ctx := logger.Context{
		Config: map[string]string{
			"awslogs-group": logGroup,
		},
		ContainerID:         container.ID,
		ContainerName:       container.Name,
		ContainerEntrypoint: container.Path,
		ContainerArgs:       container.Args,
		ContainerImageID:    container.Image,
		ContainerImageName:  container.Config.Image,
		ContainerCreated:    container.Created,
		ContainerEnv:        container.Config.Env,
		ContainerLabels:     container.Config.Labels,
	}

	logger, err := awslogs.New(ctx)

	if err != nil {
		return logger, err
	}

	m.loggers[container.ID] = logger

	return logger, nil
}

func (m *Monitor) streamLogs() {
	Kinesis := kinesis.New(&aws.Config{})

	for _ = range time.Tick(100 * time.Millisecond) {
		for _, stream := range m.streams() {
			l := m.getLines(stream)

			if l == nil {
				continue
			}

			// emit telemetry about how many lines total we've seen from the Docker API
			// These metrics can be compared to CloudWatch IncomingLogEvents and IncomingRecords
			// to understand log delivery rate.

			// extract app name from kinesis
			// myapp-staging-Kinesis-L6MUKT1VH451 -> myapp-staging
			app := stream
			parts := strings.Split(stream, "-")
			if len(parts) > 2 {
				app = strings.Join(parts[0:len(parts)-2], "-") // drop -Kinesis-YXXX
			}

			m.logSystemMetric("container streamLogs", fmt.Sprintf("dim#app=%s count#Lines=%d", app, len(l)), false)

			records := &kinesis.PutRecordsInput{
				Records:    make([]*kinesis.PutRecordsRequestEntry, len(l)),
				StreamName: aws.String(stream),
			}

			for i, line := range l {
				records.Records[i] = &kinesis.PutRecordsRequestEntry{
					Data:         line,
					PartitionKey: aws.String(string(time.Now().UnixNano())),
				}
			}

			res, err := Kinesis.PutRecords(records)

			if err != nil {
				m.logSystemMetric("container streamLogs", fmt.Sprintf("stream=%s count#KinesisPutRecordsError=1 err=%q", stream, err), false)
			}

			errorCount := 0
			errorMsg := ""

			for _, r := range res.Records {
				if r.ErrorCode != nil {
					errorCount += 1
					errorMsg = fmt.Sprintf("%s - %s", *r.ErrorCode, *r.ErrorMessage)
				}
			}

			m.logSystemMetric("container streamLogs", fmt.Sprintf("stream=%s count#KinesisRecordsSuccesses=%d count#KinesisRecordsErrors=%d err=%q", stream, len(res.Records), errorCount, errorMsg), false)
		}
	}
}

func (m *Monitor) addLine(stream string, data []byte) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.lines[stream] = append(m.lines[stream], data)
}

func (m *Monitor) getLines(stream string) [][]byte {
	m.lock.Lock()
	defer m.lock.Unlock()

	nl := len(m.lines[stream])

	if nl == 0 {
		return nil
	}

	if nl > 500 {
		nl = 500
	}

	ret := make([][]byte, nl)
	copy(ret, m.lines[stream])
	m.lines[stream] = m.lines[stream][nl:]

	return ret
}

func (m *Monitor) streams() []string {
	m.lock.Lock()
	defer m.lock.Unlock()

	streams := make([]string, len(m.lines))
	i := 0

	for key, _ := range m.lines {
		streams[i] = key
		i += 1
	}

	return streams
}
