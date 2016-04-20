package main

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/awslogs"
	docker "github.com/fsouza/go-dockerclient"
)

func (m *Monitor) Containers() {
	fmt.Printf("container at=start\n")

	m.handleRunning()
	m.handleExited()

	ch := make(chan *docker.APIEvents)

	go m.handleEvents(ch)
	go m.streamLogs()

	m.client.AddEventListener(ch)
}

// List already running containers and subscribe and stream logs
func (m *Monitor) handleRunning() {
	fmt.Printf("container handleRunning at=start\n")

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

		fmt.Printf("container handleRunning id=%s\n", shortId)
		m.handleCreate(container.ID)
	}

	fmt.Printf("container handleRunning at=end\n")
}

// List already exiteded containers and remove
func (m *Monitor) handleExited() {
	fmt.Printf("container handleExited at=start\n")

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

		fmt.Printf("container handleExited id=%s\n", shortId)
		m.handleDie(container.ID)
	}

	fmt.Printf("container handleExited at=exit\n")
}

func (m *Monitor) handleEvents(ch chan *docker.APIEvents) {
	for event := range ch {

		shortId := event.ID

		if len(shortId) > 12 {
			shortId = shortId[0:12]
		}

		switch event.Status {
		case "create":
			go m.handleCreate(event.ID)
		case "die":
			go m.handleDie(event.ID)
		case "kill":
			go m.handleKill(event.ID)
		case "oom":
			go m.handleOom(event.ID)
		case "start":
			go m.handleStart(event.ID)
		case "stop":
			go m.handleStop(event.ID)
		}

		metric := "DockerEvent" + ucfirst(event.Status)
		msg := fmt.Sprintf("container handleEvents id=%s time=%d count#%s=1", event.ID, event.Time, metric)

		if env, ok := m.envs[event.ID]; ok {
			if p := env["PROCESS"]; p != "" {
				msg = fmt.Sprintf("container handleEvents id=%s process=%s time=%d count#%s=1", event.ID, p, event.Time, metric)
			}
		}

		m.logSystemf(msg)
	}
}

// handleCreate inspects a created or existing container
// It extracts env, and creates an awslogger that will be used later
func (m *Monitor) handleCreate(id string) {
	env := map[string]string{}

	container, err := m.client.InspectContainer(id)
	if err != nil {
		m.logSystemf("container handleCreate client.inspectContainer count#DockerInspectError=1 err=%q", err)
		return
	}

	for _, e := range container.Config.Env {
		parts := strings.SplitN(e, "=", 2)

		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}

	m.envs[id] = env

	// create a an awslogger and associated CloudWatch Logs LogGroup
	if env["LOG_GROUP"] != "" {
		awslogger, aerr := m.StartAWSLogger(container, env["LOG_GROUP"])
		if aerr != nil {
			m.logSystemf("container handleCreate StartAWSLogger logGroup=%s process=%s err=%q", env["LOG_GROUP"], env["PROCESS"], err)
		} else {
			m.logSystemf("container handleCreate StartAWSLogger logGroup=%s process=%s", env["LOG_GROUP"], env["PROCESS"])
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

	msg := fmt.Sprintf("Dead process %s", id[0:12])

	if p := m.envs[id]["PROCESS"]; p != "" {
		msg = fmt.Sprintf("Dead %s process %s", p, id[0:12])
	}

	m.logAppEvent(id, msg)
}

func (m *Monitor) handleKill(id string) {
	msg := fmt.Sprintf("Stopped process %s via SIGKILL", id[0:12])

	if p := m.envs[id]["PROCESS"]; p != "" {
		msg = fmt.Sprintf("Stopped %s process %s via SIGKILL", p, id[0:12])
	}

	m.logAppEvent(id, msg)
}

func (m *Monitor) handleOom(id string) {
	msg := fmt.Sprintf("Stopped process %s due to OOM", id[0:12])

	if p := m.envs[id]["PROCESS"]; p != "" {
		msg = fmt.Sprintf("Stopped %s process %s due to OOM", p, id[0:12])
	}

	m.logAppEvent(id, msg)
}

func (m *Monitor) handleStart(id string) {
	m.updateCgroups(id)

	if id != m.agentId {
		m.subscribeLogs(id)
	}
}

func (m *Monitor) handleStop(id string) {
	msg := fmt.Sprintf("Stopped process %s via SIGTERM", id[0:12])

	if p := m.envs[id]["PROCESS"]; p != "" {
		msg = fmt.Sprintf("Stopped %s process %s via SIGTERM", p, id[0:12])
	}

	m.logAppEvent(id, msg)
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
	fmt.Printf("container subscribeLogs id=%s at=start\n", id)

	for {
		wg := new(sync.WaitGroup)
		wg.Add(2)

		exit := make(chan bool)
		r, w := io.Pipe()

		go m.readLines(id, r, wg, exit)
		go m.followDockerLogs(id, w, wg, exit)

		wg.Wait()

		// If Docker indicates the container is no longer running, stop following logs
		// Otherwise retry optimistically in attempt to maximize log delivery
		c, err := m.client.InspectContainer(id)
		switch err := err.(type) {

		// Container state is available
		case nil:
			if !c.State.Running {
				break
			} else {
				// container is still running, record metric and retry getting logs
				fmt.Printf("container subscribeLogs id=%s count#DockerLogsRetry=1", id)
				continue
			}

		// // Container is missing. Report exception and stop
		// case docker.NoSuchContainer:
		// 	m.ReportError(err)
		// 	break

		// Container state is indeterminate. Report exception and retry
		default:
			fmt.Printf("container subscribeLogs id=%s err=%q count#DockerInspectError=1 count#DockerLogsRetry=1\n", id, err)
			m.ReportError(err)
			continue
		}
	}

	fmt.Printf("container subscribeLogs id=%s\n at=end", id)
}

func (m *Monitor) readLines(id string, r *io.PipeReader, wg *sync.WaitGroup, exit chan bool) {
	fmt.Printf("container subscribeLogs readLines id=%s at=start\n", id)

	defer wg.Done()

	br := bufio.NewReader(r)

	for {
		select {
		case <-exit:
			fmt.Printf("container subscribeLogs readLines id=%s at=end exit=true\n", id)
			return
		default:
			line, err := br.ReadString('\n')
			if err != nil && err != io.EOF {
				fmt.Printf("container subscribeLogs readLines id=%s at=end err=%q\n", id, err)
				return
			} else if line != "" {
				m.parseAndForwardLine(id, line)
			}
		}
	}
}

func (m *Monitor) followDockerLogs(id string, w *io.PipeWriter, wg *sync.WaitGroup, exit chan bool) {
	fmt.Printf("container subscribeLogs followDockerLogs id=%s at=start\n", id)

	defer wg.Done()

	err := m.client.Logs(docker.LogsOptions{
		Since:        time.Now().Unix(),
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

	if err != nil {
		fmt.Printf("container subscribeLogs followDockerLogs id=%s count#DockerLogsError=1\n", id)
	}

	close(exit)

	fmt.Printf("container subscribeLogs followDockerLogs id=%s at=end\n", id)
}

func (m *Monitor) parseAndForwardLine(id, line string) {
	line = line[0 : len(line)-1] // trim off trailing newline from ReadString

	// split and parse docker timestamp
	ts := time.Now()

	parts := strings.SplitN(line, " ", 2)
	if len(parts) == 2 {
		t, err := time.Parse(time.RFC3339Nano, parts[0])
		if err != nil {
			fmt.Printf("container subscribeLogs parseAndForwardLine time.Parse err=%q\n", err)
		} else {
			ts = t
			line = parts[1]
		}
	}

	env := m.envs[id]

	app := env["APP"]
	kinesis := env["KINESIS"]
	logGroup := env["LOG_GROUP"]
	process := env["PROCESS"]
	release := env["RELEASE"]

	// if APP is not available for legacy reasons, fall back to inferring from LOG_GROUP or KINESIS
	if app == "" {
		logResource := logGroup
		if logGroup == "" {
			logResource = kinesis
		}

		// extract app name from log resource
		// convox-httpd-LogGroup-1KIJO8SS9F3Q9 -> convox-httpd
		// myapp-staging-Kinesis-L6MUKT1VH451 -> myapp-staging
		parts := strings.Split(logResource, "-")
		if len(parts) > 2 {
			app = strings.Join(parts[0:len(parts)-2], "-") // drop -LogGroup-YXXX
		}
	}

	fmt.Printf("container subscribeLogs parseAndForwardLine dim#app=%s count#Lines=1\n", app)

	// append syslog-ish prefix:
	// web:RXZMCQEPDKO/1d11a78279e0 Hello from Docker.
	l := fmt.Sprintf("%s:%s/%s %s", process, release, id[0:12], line)

	if awslogger, ok := m.loggers[id]; ok {
		awslogger.Log(&logger.Message{
			ContainerID: id,
			Line:        []byte(l),
			Timestamp:   ts,
		})
	}

	if k := env["KINESIS"]; k != "" {
		// add timestamp to kinesis for legacy purposes
		m.addLine(k, []byte(fmt.Sprintf("%s %s", ts.Format("2006-01-02 15:04:05"), l)))
	}
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
				m.logSystemf("container streamLogs stream=%s count#KinesisPutRecordsError=1 err=%q", stream, err)
			}

			errorCount := 0
			errorMsg := ""

			for _, r := range res.Records {
				if r.ErrorCode != nil {
					errorCount += 1
					errorMsg = fmt.Sprintf("%s - %s", *r.ErrorCode, *r.ErrorMessage)
				}
			}

			m.logSystemf("container streamLogs stream=%s count#KinesisRecordsSuccesses=%d count#KinesisRecordsErrors=%d err=%q", stream, len(res.Records), errorCount, errorMsg)
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
