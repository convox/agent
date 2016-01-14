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

	"github.com/convox/agent/Godeps/_workspace/src/github.com/Sirupsen/logrus"
	"github.com/convox/agent/Godeps/_workspace/src/github.com/aws/aws-sdk-go/aws"
	"github.com/convox/agent/Godeps/_workspace/src/github.com/aws/aws-sdk-go/aws/awserr"
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
	logrus.WithFields(logrus.Fields{
		"_fn":         "NewMonitor",
		"AWS_REGION":  os.Getenv("AWS_REGION"),
		"DOCKER_HOST": os.Getenv("DOCKER_HOST"),
		"KINESIS":     os.Getenv("KINESIS"),
		"LOG_GROUP":   os.Getenv("LOG_GROUP"),
	}).Info()

	client, err := docker.NewClient(os.Getenv("DOCKER_HOST"))

	if err != nil {
		log.Fatal(err)
	}

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
	logrus.WithFields(logrus.Fields{
		"_fn": "Listen",
	}).Info()

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
	logrus.WithFields(logrus.Fields{
		"_fn": "handleRunning",
	}).Info()

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
	logrus.WithFields(logrus.Fields{
		"_fn": "handleRunning",
	}).Info()

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
	logrus.WithFields(logrus.Fields{
		"_fn": "handleEvents",
		"ch":  ch,
	}).Info()

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
	logrus.WithFields(logrus.Fields{
		"_fn": "handleCreate",
		"id":  id,
	}).Info()

	env, err := m.inspectContainer(id)

	if err != nil {
		log.Printf("error: %s\n", err)
		return
	}

	m.setEnv(id, env)

	m.logEvent(id, fmt.Sprintf("Starting process %s", id[0:12]))

	go m.subscribeLogs(id)
}

func (m *Monitor) handleDie(id string) {
	logrus.WithFields(logrus.Fields{
		"_fn": "handleDie",
		"id":  id,
	}).Info()

	// While we could remove a container and volumes on this event
	// It seems like explicitly doing a `docker run --rm` is the best way
	// to state this intent.
	m.logEvent(id, fmt.Sprintf("Dead process %s", id[0:12]))
}

func (m *Monitor) handleKill(id string) {
	logrus.WithFields(logrus.Fields{
		"_fn": "handleKill",
		"id":  id,
	}).Info()

	m.logEvent(id, fmt.Sprintf("Stopped process %s via SIGKILL", id[0:12]))
}

func (m *Monitor) handleStart(id string) {
	logrus.WithFields(logrus.Fields{
		"_fn": "handleStart",
		"id":  id,
	}).Info()

	m.updateCgroups(id)
}

func (m *Monitor) handleStop(id string) {
	logrus.WithFields(logrus.Fields{
		"_fn": "handleStop",
		"id":  id,
	}).Info()

	m.logEvent(id, fmt.Sprintf("Stopped process %s via SIGTERM", id[0:12]))
}

func (m *Monitor) inspectContainer(id string) (map[string]string, error) {
	logrus.WithFields(logrus.Fields{
		"_fn": "inspectContainer",
		"id":  id,
	}).Info()

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

// Get log resource from app environment. Prefer CloudWatch LogGroup over Kinesis.
func (m *Monitor) logResource(id string) string {
	logrus.WithFields(logrus.Fields{
		"_fn": "logResource",
		"id":  id,
	}).Info()

	env := m.getEnv(id)

	logGroup := env["LOG_GROUP"]
	kinesis := env["KINESIS"]

	if logGroup != "" {
		return logGroup
	}

	return kinesis
}

// Log an agent event to the container Log Group or Kinesis
func (m *Monitor) logEvent(id, message string) {
	logrus.WithFields(logrus.Fields{
		"_fn":     "logEvent",
		"id":      id,
		"message": message,
	}).Info()

	logResource := m.logResource(id)

	if logResource != "" {
		m.addLine(id, []byte(fmt.Sprintf("%s %s %s : %s", time.Now().Format("2006-01-02 15:04:05"), m.instanceId, m.image, message)))
	}
}

// Log an internal agent event to stdout
// Optionally put the message into a Log Group queue.
// Use the instanceId instead since there isn't necessarily a convox/web container running here
func (m *Monitor) logInternalEvent(message string, put bool) {
	logrus.WithFields(logrus.Fields{
		"_fn":     "logInternalEvent",
		"message": message,
		"put":     put,
	}).Info()

	line := fmt.Sprintf("%s %s %s : %s", time.Now().Format("2006-01-02 15:04:05"), m.instanceId, m.image, message)

	fmt.Println(line)

	if put {
		m.addLine(m.instanceId, []byte(line))
	}
}

func (m *Monitor) logInternalAWSErr(err error, msg string) {
	if awsErr, ok := err.(awserr.Error); ok {
		logrus.WithFields(logrus.Fields{
			"errorCode": awsErr.Code(),
			"message":   awsErr.Message(),
			"origError": awsErr.OrigErr(),
			// "logGroupName":  l.logGroupName,
			// "logStreamName": l.logStreamName,
			"count#awserr": 1,
		}).Error(msg)

		m.addLine(m.instanceId, []byte(fmt.Sprintf("_fn=logInternalAWSErr errorCode=%q message=%q origError=%q count#awserr=1 msg=%q", awsErr.Code(), awsErr.Message(), awsErr.OrigErr(), msg)))
	}
}

// Modify the container cgroup to enable swap if SWAP=1 is set
// Currently this only works on the Amazon ECS AMI, not Docker Machine and boot2docker
// until a better strategy for knowing where the cgroup mount is implemented
func (m *Monitor) updateCgroups(id string) {
	logrus.WithFields(logrus.Fields{
		"_fn": "updateCgroups",
		"id":  id,
	}).Info()

	env := m.getEnv(id)

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
	logrus.WithFields(logrus.Fields{
		"_fn": "subscribeLogs",
		"id":  id,
	}).Info()

	env := m.getEnv(id)

	logResource := m.logResource(id)

	if logResource == "" {
		return
	}

	process := env["PROCESS"]
	release := env["RELEASE"]

	// extract app name from Log Resource
	// myapp-staging-LogGroup-9I65CAJ6OLO9 -> myapp-staging
	app := ""

	parts := strings.Split(logResource, "-")
	if len(parts) > 2 {
		app = strings.Join(parts[0:len(parts)-2], "-") // drop -LogGroup-YXXX
	}

	time.Sleep(500 * time.Millisecond)

	r, w := io.Pipe()

	go func(prefix string, r io.ReadCloser) {
		logrus.WithFields(logrus.Fields{
			"_fn":    "subscribeLogs go func",
			"prefix": prefix,
			"r":      r,
		}).Info()

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
	logrus.WithFields(logrus.Fields{
		"_fn": "putKinesisLogs",
		"id":  id,
		"l":   elide(l[0]),
	}).Info()

	Kinesis := kinesis.New(&aws.Config{})

	env := m.getEnv(id)

	stream := env["KINESIS"]

	if stream == "" {
		return
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
		m.logInternalAWSErr(err, "Kinesis.PutRecords")
	}

	for _, r := range res.Records {
		if r.ErrorCode != nil {
			fmt.Printf("error: %s\n", *r.ErrorCode)
		}
	}

	fmt.Printf("monitor upload to=kinesis stream=%q lines=%d\n", stream, len(res.Records))
}

func (m *Monitor) putCloudWatchLogs(id string, l [][]byte) {
	logrus.WithFields(logrus.Fields{
		"_fn": "putCloudWatchLogs",
		"id":  id,
		"l":   elide(l[0]),
	}).Info()

	Logs := cloudwatchlogs.New(&aws.Config{})

	env := m.getEnv(id)

	logGroup := env["LOG_GROUP"]
	process := env["PROCESS"]

	putInternal := true

	// logConvoxEvent uses the instance id for logs that should end up in the convox log group
	if id == m.instanceId {
		logGroup = os.Getenv("LOG_GROUP")
		process = "web"
		putInternal = false
	}

	if logGroup == "" {
		return
	}

	streamName := fmt.Sprintf("%s/%s", process, id)

	// describe the LogStream and sequence token
	// or create a LogStream if doesn't exist
	if m.sequenceTokens[streamName] == "" {
		res, err := Logs.DescribeLogStreams(&cloudwatchlogs.DescribeLogStreamsInput{
			LogGroupName:        aws.String(logGroup),
			LogStreamNamePrefix: aws.String(streamName),
		})

		fmt.Printf("ns=agent at=putCloudWatchLogs.DescribeLogStreams group=%s stream=%s\n", logGroup, streamName)

		if err != nil {
			m.logInternalAWSErr(err, "Logs.DescribeLogStreams")

			m.logInternalEvent(
				fmt.Sprintf("ns=agent at=putCloudWatchLogs.DescribeLogStreams group=%s stream=%s count#error.DescribeLogStreams=1 msg=%q", logGroup, streamName, err.Error()),
				putInternal,
			)

			return
		}

		if len(res.LogStreams) == 0 {
			_, err := Logs.CreateLogStream(&cloudwatchlogs.CreateLogStreamInput{
				LogGroupName:  aws.String(logGroup),
				LogStreamName: aws.String(streamName),
			})

			if err != nil {
				m.logInternalAWSErr(err, "Logs.CreateLogStream")

				m.logInternalEvent(
					fmt.Sprintf("ns=agent at=putCloudWatchLogs.CreateLogStream group=%s stream=%s count#error.CreateLogStream=1 msg=%q", logGroup, streamName, err.Error()),
					putInternal,
				)

				return
			}
		} else {
			for _, s := range res.LogStreams {
				m.setSequenceToken(*s.LogStreamName, *s.UploadSequenceToken)
			}
		}
	}

	logs := &cloudwatchlogs.PutLogEventsInput{
		LogGroupName:  aws.String(logGroup),
		LogStreamName: aws.String(streamName),
		LogEvents:     make([]*cloudwatchlogs.InputLogEvent, len(l)),
	}

	token := m.getSequenceToken(streamName)

	if token != "" {
		logs.SequenceToken = aws.String(token)
	}

	for i, line := range l {
		logs.LogEvents[i] = &cloudwatchlogs.InputLogEvent{
			Message:   aws.String(string(line)),
			Timestamp: aws.Long(time.Now().UnixNano() / 1000 / 1000), // ms since epoch
		}
	}

	pres, err := Logs.PutLogEvents(logs)

	fmt.Printf("ns=agent at=putCloudWatchLogs.PutLogEvents group=%s stream=%s events=%d sequenceToken=%s\n", logGroup, streamName, len(logs.LogEvents), m.sequenceTokens[streamName])

	if err != nil {
		m.logInternalAWSErr(err, "Logs.PutLogEvents")

		m.logInternalEvent(
			fmt.Sprintf("ns=agent at=putCloudWatchLogs.PutLogEvents group=%s stream=%s count#error.PutLogEvents=1 msg=%q", logGroup, streamName, err.Error()),
			putInternal,
		)
		return
	}

	m.sequenceTokens[streamName] = *pres.NextSequenceToken

	m.logInternalEvent(
		fmt.Sprintf("ns=agent at=putCloudWatchLogs.PutLogEvents group=%s count#logevents=%d", logGroup, len(logs.LogEvents)),
		putInternal,
	)
}

func (m *Monitor) putLogs() {
	logrus.WithFields(logrus.Fields{
		"_fn": "putLogs",
	}).Info()

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

func (m *Monitor) setEnv(id string, env map[string]string) {
	logrus.WithFields(logrus.Fields{
		"_fn": "setEnv",
		"id":  id,
		"env": env,
	}).Info()

	m.lock.Lock()
	defer m.lock.Unlock()

	m.envs[id] = env
}

func (m *Monitor) getEnv(id string) map[string]string {
	logrus.WithFields(logrus.Fields{
		"_fn": "getEnv",
		"id":  id,
	}).Info()

	m.lock.Lock()
	defer m.lock.Unlock()

	return m.envs[id]
}

func (m *Monitor) setSequenceToken(streamName, token string) {
	logrus.WithFields(logrus.Fields{
		"_fn":        "setSequenceToken",
		"streamName": streamName,
		"token":      token,
	}).Info()

	m.lock.Lock()
	defer m.lock.Unlock()

	m.sequenceTokens[streamName] = token
}

func (m *Monitor) getSequenceToken(streamName string) string {
	logrus.WithFields(logrus.Fields{
		"_fn":        "getSequenceToken",
		"streamName": streamName,
	}).Info()

	m.lock.Lock()
	defer m.lock.Unlock()

	return m.sequenceTokens[streamName]
}

func (m *Monitor) addLine(id string, data []byte) {
	logrus.WithFields(logrus.Fields{
		"_fn":  "addLine",
		"id":   id,
		"data": elide(data),
	}).Info()

	m.lock.Lock()
	defer m.lock.Unlock()

	m.lines[id] = append(m.lines[id], data)
}

func elide(data []byte) string {
	s := string(data)
	if len(s) < 20 {
		return s
	}

	return s[0:20] + "..."
}

func (m *Monitor) getLines(id string) [][]byte {
	// logrus.WithFields(logrus.Fields{
	// 	"_fn": "getLines",
	// 	"id": id,
	// }).Info()

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
	// logrus.WithFields(logrus.Fields{
	// 	"_fn": "ids",
	// }).Info()

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
