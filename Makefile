.PHONY: test fmt broker relay volunteer relayhub client docker-build docker-keygen docker-run relayhub-docker-build broker-docker-build

VOLUNTEER_IMAGE ?= openrung-volunteer:latest
RELAYHUB_IMAGE ?= openrung-relayhub:latest
BROKER_IMAGE ?= openrung-broker:latest

fmt:
	gofmt -w cmd internal punchcore

test:
	go test ./...
	cd punchcore && go test ./...

broker:
	OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true go run ./cmd/broker -addr :8080

relay:
	go run ./cmd/volunteer \
		-broker http://localhost:8080 \
		-public-host 127.0.0.1 \
		-public-port 443 \
		-client-id 2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff \
		-reality-private-key dev-private-key \
		-reality-public-key dev-public-key \
		-short-id 5f7a8d9c01ab23cd \
		-skip-xray-run

# Legacy local-development alias; the executable path is migrated separately.
volunteer: relay

relayhub:
	OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true go run ./cmd/relayhub \
		-broker http://localhost:8080 \
		-public-host 127.0.0.1 \
		-port-range 20000-20010 \
		-control-addr :9443

client:
	go run ./cmd/client check -broker http://localhost:8080

# Build the relay runtime image (legacy target/image names retained for compatibility).
docker-build:
	docker build -f deploy/volunteer/Dockerfile -t $(VOLUNTEER_IMAGE) .

# Build the relay hub image (run from the repo root).
relayhub-docker-build:
	docker build -f deploy/relayhub/Dockerfile -t $(RELAYHUB_IMAGE) .

# Build the broker (control plane) image (run from the repo root).
broker-docker-build:
	docker build -f deploy/broker/Dockerfile -t $(BROKER_IMAGE) .

# Print a fresh Reality key pair for a stable relay identity.
docker-keygen:
	docker run --rm --entrypoint xray $(VOLUNTEER_IMAGE) x25519

# Run the relay using deploy/volunteer/.env (host networking, least privilege).
docker-run:
	docker run --rm --network host \
		--cap-drop ALL --cap-add NET_BIND_SERVICE \
		--read-only --tmpfs /tmp \
		--env-file deploy/volunteer/.env $(VOLUNTEER_IMAGE)
