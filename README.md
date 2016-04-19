# convox/agent

Instance agent for collecting logs and metrics.

## Development

Copy select outputs from a dev rack into .env:

```bash
$ cat .env
AWS_REGION=us-west-2
AWS_ACCESS_KEY_ID=XXX
AWS_SECRET_ACCESS_KEY=YYY
KINESIS=convox-Kinesis-2NQ3Q5ASHY1N
LOG_GROUP=convox-LogGroup-9I65CAJ6OLO9
```

This approximates the IAM role and env the agent has on a cluster instance.

Start a dev agent:

```bash
$ convox start
agent | monitor new region=us-west-2 cluster=convox-Cluster-1NCWX9EC0JOV4
agent | disk monitor hostname=4f7ca7a9232a
agent | disk monitor hostname=4f7ca7a9232a utilization=16.02% used=0.1209G available=0.7547G
agent | disk monitor upload to=kinesis stream="convox-Kinesis-2NQ3Q5ASHY1N" lines=1
```

Run a Docker container to see Docker event Kinesis and CloudWatch Logs upload activity:

```bash
$ docker run -e KINESIS=convox-Kinesis-8WL8ZDHOGV5F -e LOG_GROUP=convox-LogGroup-V21KNCGSV61R -e PROCESS=hello-world -e RELEASE=RXBKPDQEGDU hello-world
```

```
agent | monitor event id=a5018a56adc3 status=create
agent | monitor event id=a5018a56adc3 status=attach
agent | monitor event id=a5018a56adc3 status=start
agent | monitor event id=a5018a56adc3 status=die
agent | monitor upload to=kinesis stream="convox-Kinesis-2NQ3Q5ASHY1N" lines=21
```

Run a Docker container to see cgroup fun:

```bash
$ docker run -m 50MB rabbitmq
Killed

$ docker run -e SWAP=1 -m 50MB rabbitmq

              RabbitMQ 3.6.0. Copyright (C) 2007-2015 Pivotal Software, Inc.
  ##  ##      Licensed under the MPL.  See http://www.rabbitmq.com/
  ##  ##
  ##########  Logs: tty
  ######  ##        tty
  ##########
              Starting broker...
...
```

```
agent | monitor event id=34c639f071e9 status=create time=1456858799
agent | monitor event id=34c639f071e9 status=attach time=1456858799
agent | monitor event id=34c639f071e9 status=start time=1456858799
agent | monitor event id=34c639f071e9 status=oom time=1456858802
agent | monitor event id=34c639f071e9 status=die time=1456858803


agent | monitor event id=aadfffc88cb0 status=create time=1456858855
agent | monitor event id=aadfffc88cb0 status=attach time=1456858855
agent | monitor event id=aadfffc88cb0 status=start time=1456858855
agent | monitor cgroups id=aadfffc88cb0 cgroup=memory.memsw.limit_in_bytes value=18446744073709551615
agent | monitor cgroups id=aadfffc88cb0 cgroup=memory.soft_limit_in_bytes value=18446744073709551615
agent | monitor cgroups id=aadfffc88cb0 cgroup=memory.limit_in_bytes value=18446744073709551615
```

## Release

convox/agent is released as a public Docker image on Docker Hub, and public
config file on S3.

```bash
$ make release
```

## Production

convox/agent runs on every ECS cluster instance. This is configured by the
convox/rack CloudFormation template UserData and an upstart script.

Pseudocode:

```bash
$ mkdir -p /etc/convox
$ echo us-east-1 > /etc/convox/region
$ curl -s http://convox.s3.amazonaws.com/agent/0.3/convox.conf > /etc/init/convox.conf
$ start convox

# Spawns
$ docker run -a STDOUT -a STDERR --sig-proxy -e AWS_REGION=$(cat /etc/convox/region) -v /cgroup:/cgroup -v /var/run/docker.sock:/var/run/docker.sock convox/agent:0.3
```

The running agent can

* Watch Docker events via the host Docker socket
* Modify contaier settings via the host /cgroup control groups
* Put events (logs) to Kinesis streams via the InstanceProfile
* Put metric data to CloudWatch via the InstanceProfile

## License

Apache 2.0 &copy; 2015 Convox, Inc.
