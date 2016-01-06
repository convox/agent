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

	"github.com/convox/agent/Godeps/_workspace/src/github.com/aws/aws-sdk-go/aws"
	"github.com/convox/agent/Godeps/_workspace/src/github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/convox/agent/Godeps/_workspace/src/github.com/aws/aws-sdk-go/service/kinesis"
	docker "github.com/convox/agent/Godeps/_workspace/src/github.com/fsouza/go-dockerclient"
)

type Monitor struct {
	client *docker.Client

	instanceId string
	image      string

	envs map[string]map[string]string // container id -> env on create

	lock           sync.Mutex          // lock around Docker logs -> lines and lines -> AWS
	lines          map[string][][]byte // container id -> logs that is truncated on PUT
	sequenceTokens map[string]string   // container id (LogStream) -> SequenceToken that is updated on PUT
}

func NewMonitor() *Monitor {
	client, err := docker.NewClient(os.Getenv("DOCKER_HOST"))

	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("monitor new region=%s kinesis=%s log_group=%s\n", os.Getenv("AWS_REGION"), os.Getenv("KINESIS"), os.Getenv("LOG_GROUP"))

	return &Monitor{
		client: client,

		instanceId: GetInstanceId(),
		image:      "convox/agent", // also set during handleRunning

		envs: make(map[string]map[string]string),

		lines:          make(map[string][][]byte),
		sequenceTokens: make(map[string]string),
	}
}

func (m *Monitor) Listen() {
	m.handleRunning()
	m.handleExited()

	ch := make(chan *docker.APIEvents)

	go m.handleEvents(ch)
	go m.putLogs()

	m.client.AddEventListener(ch)

	for {
		time.Sleep(60 * time.Second)
	}
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
			m.image = img
			fmt.Printf("monitor event id=%s status=skipped\n", shortId)
			continue
		}

		fmt.Printf("monitor event id=%s status=created\n", shortId)
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

		fmt.Printf("monitor event id=%s status=%s time=%d\n", shortId, event.Status, event.Time)

		switch event.Status {
		case "create":
			m.handleCreate(event.ID)
		case "die":
			m.handleDie(event.ID)
		case "kill":
			m.handleKill(event.ID)
		case "start":
			m.handleStart(event.ID)
		case "stop":
			m.handleStop(event.ID)
		}
	}
}

func (m *Monitor) handleCreate(id string) {
	env, err := m.inspectContainer(id)

	if err != nil {
		log.Printf("error: %s\n", err)
		return
	}

	m.envs[id] = env

	m.logEvent(id, fmt.Sprintf("Starting process %s", id[0:12]))

	go m.subscribeLogs(id)
}

func (m *Monitor) handleDie(id string) {
	// While we could remove a container and volumes on this event
	// It seems like explicitly doing a `docker run --rm` is the best way
	// to state this intent.
	m.logEvent(id, fmt.Sprintf("Dead process %s", id[0:12]))
}

func (m *Monitor) handleKill(id string) {
	m.logEvent(id, fmt.Sprintf("Stopped process %s via SIGKILL", id[0:12]))
}

func (m *Monitor) handleStart(id string) {
	m.updateCgroups(id, m.envs[id])
}

func (m *Monitor) handleStop(id string) {
	m.logEvent(id, fmt.Sprintf("Stopped process %s via SIGTERM", id[0:12]))
}

func (m *Monitor) inspectContainer(id string) (map[string]string, error) {
	env := map[string]string{}

	container, err := m.client.InspectContainer(id)

	if err != nil {
		log.Printf("error: %s\n", err)
		return env, err
	}

	for _, e := range container.Config.Env {
		parts := strings.SplitN(e, "=", 2)

		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}

	return env, nil
}

func (m *Monitor) logEvent(id, message string) {
	env := m.envs[id]
	logGroup := env["LOG_GROUP"]

	if logGroup != "" {
		m.addLine(id, []byte(fmt.Sprintf("%s %s %s : %s", time.Now().Format("2006-01-02 15:04:05"), m.instanceId, m.image, message)))
	}
}

// Modify the container cgroup to enable swap if SWAP=1 is set
// Currently this only works on the Amazon ECS AMI, not Docker Machine and boot2docker
// until a better strategy for knowing where the cgroup mount is implemented
func (m *Monitor) updateCgroups(id string, env map[string]string) {
	if env["SWAP"] == "1" {
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

	logGroup := env["LOG_GROUP"]
	process := env["PROCESS"]
	release := env["RELEASE"]

	if logGroup == "" {
		return
	}

	// extract app name from LogGroup
	// myapp-staging-LogGroup-9I65CAJ6OLO9 -> myapp-staging
	app := ""

	parts := strings.Split(logGroup, "-")
	if len(parts) > 2 {
		app = strings.Join(parts[0:len(parts)-2], "-") // drop -LogGroup-YXXX
	}

	time.Sleep(500 * time.Millisecond)

	r, w := io.Pipe()

	go func(prefix string, r io.ReadCloser) {
		defer r.Close()

		scanner := bufio.NewScanner(r)

		for scanner.Scan() {
			m.addLine(id, []byte(fmt.Sprintf("%s %s %s/%s:%s : %s", time.Now().Format("2006-01-02 15:04:05"), m.instanceId, app, process, release, scanner.Text())))
		}

		if scanner.Err() != nil {
			log.Printf("error: %s\n", scanner.Err())
		}
	}(process, r)

	err := m.client.Logs(docker.LogsOptions{
		Container:    id,
		Follow:       true,
		Stdout:       true,
		Stderr:       true,
		Tail:         "all",
		RawTerminal:  false,
		OutputStream: w,
		ErrorStream:  w,
	})

	if err != nil {
		log.Printf("error: %s\n", err)
	}

	w.Close()
}

func (m *Monitor) putKinesisLogs(id string, l [][]byte) {
	Kinesis := kinesis.New(&aws.Config{})

	stream := m.envs[id]["KINESIS"]

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
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
	}

	for _, r := range res.Records {
		if r.ErrorCode != nil {
			fmt.Printf("error: %s\n", *r.ErrorCode)
		}
	}

	fmt.Printf("monitor upload to=kinesis stream=%q lines=%d\n", stream, len(res.Records))
}

func (m *Monitor) putCloudWatchLogs(id string, l [][]byte) {
	Logs := cloudwatchlogs.New(&aws.Config{})

	logGroup := m.envs[id]["LOG_GROUP"]
	process := m.envs[id]["PROCESS"]

	streamName := fmt.Sprintf("%s/%s", process, id)

	// describe the LogStream and sequence token
	// or create a LogStream if doesn't exist
	if m.sequenceTokens[id] == "" {
		res, err := Logs.DescribeLogStreams(&cloudwatchlogs.DescribeLogStreamsInput{
			LogGroupName:        aws.String(logGroup),
			LogStreamNamePrefix: aws.String(streamName),
		})

		if err != nil {
			fmt.Printf("error: %s\n", err)
			return
		}

		if len(res.LogStreams) == 0 {
			_, err := Logs.CreateLogStream(&cloudwatchlogs.CreateLogStreamInput{
				LogGroupName:  aws.String(logGroup),
				LogStreamName: aws.String(streamName),
			})

			if err != nil {
				fmt.Printf("error: %s\n", err)
				return
			}
		} else {
			for _, s := range res.LogStreams {
				m.sequenceTokens[*s.LogStreamName] = *s.UploadSequenceToken
			}
		}
	}

	logs := &cloudwatchlogs.PutLogEventsInput{
		LogGroupName:  aws.String(logGroup),
		LogStreamName: aws.String(streamName),
		LogEvents:     make([]*cloudwatchlogs.InputLogEvent, len(l)),
	}

	if token, ok := m.sequenceTokens[id]; ok {
		logs.SequenceToken = aws.String(token)
	}

	for i, line := range l {
		logs.LogEvents[i] = &cloudwatchlogs.InputLogEvent{
			Message:   aws.String(string(line)),
			Timestamp: aws.Long(time.Now().UnixNano() / 1000 / 1000), // ms since epoch
		}
	}

	pres, err := Logs.PutLogEvents(logs)

	if err != nil {
		fmt.Printf("error: %s\n", err)
		return
	}

	m.sequenceTokens[id] = *pres.NextSequenceToken

	fmt.Printf("monitor upload to=cloudwatchlogs log_group=%s log_stream=%s lines=%d rejected=%+v\n", logGroup, streamName, len(logs.LogEvents), pres.RejectedLogEventsInfo)
}

func (m *Monitor) putLogs() {
	for _ = range time.Tick(100 * time.Millisecond) {
		for _, id := range m.ids() {

			l := m.getLines(id)

			if l == nil {
				continue
			}

			m.putCloudWatchLogs(id, l)
			m.putKinesisLogs(id, l)
		}
	}
}

func (m *Monitor) addLine(id string, data []byte) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.lines[id] = append(m.lines[id], data)
}

func (m *Monitor) getLines(id string) [][]byte {
	m.lock.Lock()
	defer m.lock.Unlock()

	nl := len(m.lines[id])

	if nl == 0 {
		return nil
	}

	if nl > 500 {
		nl = 500
	}

	ret := make([][]byte, nl)
	copy(ret, m.lines[id])
	m.lines[id] = m.lines[id][nl:]

	return ret
}

func (m *Monitor) ids() []string {
	m.lock.Lock()
	defer m.lock.Unlock()

	ids := make([]string, len(m.lines))
	i := 0

	for key, _ := range m.lines {
		ids[i] = key
		i += 1
	}

	return ids
}
