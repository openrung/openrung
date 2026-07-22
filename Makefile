.PHONY: test fmt broker relay relayhub client docker-build docker-keygen docker-run relayhub-docker-build broker-docker-build

RELAY_IMAGE ?= openrung-relay:latest
RELAYHUB_IMAGE ?= openrung-relayhub:latest
BROKER_IMAGE ?= openrung-broker:latest

fmt:
	gofmt -w cmd internal punchcore wsscore

test:
	go test ./...
	cd punchcore && go test ./...
	cd wsscore && go test ./...

broker:
	OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true go run ./cmd/broker -addr :8080

relay:
	go run ./cmd/relay \
		-broker http://localhost:8080 \
		-public-host 127.0.0.1 \
		-public-port 443 \
		-client-id 2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff \
		-reality-private-key dev-private-key \
		-reality-public-key dev-public-key \
		-short-id 5f7a8d9c01ab23cd \
		-skip-xray-run

relayhub:
	OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true go run ./cmd/relayhub \
		-broker http://localhost:8080 \
		-public-host 127.0.0.1 \
		-port-range 20000-20010 \
		-control-addr :9443

client:
	go run ./cmd/client check -broker http://localhost:8080

# Build the relay runtime image.
docker-build:
	docker build -f deploy/relay/Dockerfile -t $(RELAY_IMAGE) .

# Build the relay hub image (run from the repo root).
relayhub-docker-build:
	docker build -f deploy/relayhub/Dockerfile -t $(RELAYHUB_IMAGE) .

# Build the broker (control plane) image (run from the repo root).
broker-docker-build:
	docker build -f deploy/broker/Dockerfile -t $(BROKER_IMAGE) .

# Print a fresh Reality key pair for a stable relay identity.
docker-keygen:
	docker run --rm --entrypoint xray $(RELAY_IMAGE) x25519

# Run the relay using deploy/relay/.env (host networking, least privilege).
docker-run:
	docker run --rm --network host \
		--cap-drop ALL --cap-add NET_BIND_SERVICE \
		--read-only --tmpfs /tmp \
		--env-file deploy/relay/.env $(RELAY_IMAGE)
