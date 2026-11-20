.PHONY: test fmt broker relay relayhub client docker-build docker-keygen docker-run relayhub-docker-build broker-docker-build e2e e2e-image e2e-lab e2e-lab-host desktop-lab

RELAY_IMAGE ?= openrung-relay:latest
RELAYHUB_IMAGE ?= openrung-relayhub:latest
BROKER_IMAGE ?= openrung-broker:latest
E2E_IMAGE ?= openrung-e2e:latest
E2E_XRAY_IMAGE ?= openrung-e2e-xray:latest
# Extra `docker build` flags for the lab images (a proxy, --network host, ...).
E2E_BUILD_FLAGS ?=
# Extra arguments for the scenario suite, e.g. E2E_ARGS='-test.run Volunteer'.
E2E_ARGS ?=
E2E_RUN = docker run --rm --init --privileged --network none

fmt:
	gofmt -w brokerapi cmd connectcore desktop desktop-volunteer internal punchcore test wsscore

# The bundled sing-box runtime's build tags (internal/singboxruntime): every
# cmd/client build and root-module test run wants them, and the release
# workflow builds with them.
#   with_utls                the uTLS/Reality client — required to dial relays
#   with_external_windivert  keep sing-box's embedded WinDivert64.sys driver
#                            (LGPLv3; unused Windows bridge/tlsspoof backends)
#                            OUT of the binary and the release archives
# Root ./... excludes nested Go modules. Test the Wails service packages
# without requiring frontend assets or native webview dependencies.
test:
	go test -tags with_utls,with_external_windivert ./...
	cd brokerapi && go test ./...
	cd connectcore && go test ./...
	cd punchcore && go test ./...
	cd wsscore && go test ./...
	cd desktop && go test ./vpnservice/...
	cd desktop-volunteer && go test ./persist/... ./directsetup/... ./volunteerservice/...
	cd desktop-volunteer && go test version.go version_test.go

broker:
	OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true \
		OPENRUNG_RELAY_SIGNING_KEY="$${OPENRUNG_RELAY_SIGNING_KEY:-$$(openssl rand -base64 32)}" \
		go run ./cmd/broker -addr 127.0.0.1:8080

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
	go run -tags with_utls,with_external_windivert ./cmd/client check -broker http://localhost:8080

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

# Build the network lab image (test/e2e): every binary under test plus the
# relay image's pinned Xray stage.
e2e-image:
	docker build $(E2E_BUILD_FLAGS) -f deploy/relay/Dockerfile --target xray -t $(E2E_XRAY_IMAGE) .
	docker build $(E2E_BUILD_FLAGS) -f test/e2e/Dockerfile --build-arg XRAY_IMAGE=$(E2E_XRAY_IMAGE) -t $(E2E_IMAGE) .

# Run the end-to-end scenarios on a simulated internet inside one container.
# The container has no network of its own; --privileged lets it create the
# network namespaces the simulation is made of.
e2e: e2e-image
	$(E2E_RUN) $(E2E_IMAGE) test $(E2E_ARGS)

# Bring the lab up and leave it running; in another terminal:
#   docker exec -it openrung-lab sh    then    lab help
e2e-lab: e2e-image
	$(E2E_RUN) -it --name openrung-lab $(E2E_IMAGE) up

# The same lab, attached to this machine's network behind the lab router
# gw-host, so apps built here can use it: the broker answers on
# http://127.0.0.1:18080 and the lab's public ranges route through gw-host.
# Linux only; the attachment goes away when the lab stops.
e2e-lab-host: e2e-image
	docker run --rm --init --privileged --network host -it --name openrung-lab $(E2E_IMAGE) up -locations

# Build the desktop app from this checkout and run it against the lab that
# `make e2e-lab-host` left running.
desktop-lab:
	desktop/scripts/run-against-lab.sh
