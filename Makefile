all: build

build:
	docker build --no-cache -t convox/agent .

test:
	go test -cover -v ./...

vendor:
	godep save -r -copy=true ./...

release: build
	docker tag -f convox/agent:latest convox/agent:0.66
	docker push convox/agent:0.66
	AWS_DEFAULT_PROFILE=release aws s3 cp convox.conf s3://convox/agent/0.66/convox.conf --acl public-read
