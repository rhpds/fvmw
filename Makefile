IMAGE ?= quay.io/rhpds/fvmw
TAG ?= latest

.PHONY: build run test image push clean

build:
	go build -o bin/fvmw ./cmd/fvmw/

run: build
	FVMW_DISK_PATH=/tmp/fvmw-disks bin/fvmw -config config/default.yaml

test:
	go test ./...

vet:
	go vet ./...

image:
	podman build -t $(IMAGE):$(TAG) -f build/Containerfile .

push: image
	podman push $(IMAGE):$(TAG)

clean:
	rm -rf bin/

deploy:
	cd deploy/ansible && ansible-playbook deploy.yml -e @../../local.env

teardown:
	cd deploy/ansible && ansible-playbook deploy.yml -e @../../local.env -e fvmw_action=teardown
