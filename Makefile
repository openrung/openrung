.PHONY: test fmt broker volunteer relayhub client docker-build docker-keygen docker-run relayhub-docker-build

VOLUNTEER_IMAGE ?= openrung-volunteer:latest
RELAYHUB_IMAGE ?= openrung-relayhub:latest

fmt:
	gofmt -w cmd internal

test:
	go test ./...

broker:
	go run ./cmd/broker -addr :8080

volunteer:
	go run ./cmd/volunteer \
		-broker http://localhost:8080 \
		-public-host 127.0.0.1 \
		-public-port 443 \
		-client-id 2c08df10-4ef4-4ab9-95c6-cb1e94cdb2ff \
		-reality-private-key dev-private-key \
		-reality-public-key dev-public-key \
		-short-id 5f7a8d9c01ab23cd \
		-skip-xray-run

relayhub:
	go run ./cmd/relayhub \
		-broker http://localhost:8080 \
		-public-host 127.0.0.1 \
		-port-range 20000-20010 \
		-control-addr :9443

client:
	go run ./cmd/client check -broker http://localhost:8080

# Build the volunteer relay image (run from the repo root).
docker-build:
	docker build -f deploy/volunteer/Dockerfile -t $(VOLUNTEER_IMAGE) .

# Build the relay hub image (run from the repo root).
relayhub-docker-build:
	docker build -f deploy/relayhub/Dockerfile -t $(RELAYHUB_IMAGE) .

# Print a fresh Reality key pair for a stable relay identity.
docker-keygen:
	docker run --rm --entrypoint xray $(VOLUNTEER_IMAGE) x25519

# Run the relay using deploy/volunteer/.env (host networking, least privilege).
docker-run:
	docker run --rm --network host \
		--cap-drop ALL --cap-add NET_BIND_SERVICE \
		--read-only --tmpfs /tmp \
		--env-file deploy/volunteer/.env $(VOLUNTEER_IMAGE)
