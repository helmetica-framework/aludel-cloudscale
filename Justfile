import "Justfile.vars.just"

_default:
    @just --list

# Build the driver binary
#
# CGO is disabled here only, not globally: the image is distroless and needs a
# static binary, while `just test` runs with -race, which requires cgo.
build: fmt vet
    @echo "GOOS=$(go env GOOS) GOARCH=$(go env GOARCH)"
    CGO_ENABLED=0 go build -o {{ bin_filename }}

# Run tests
test:
    go test ./... -race -coverprofile cover.out

# Run go fmt against code
fmt:
    go fmt ./...

# Run go vet against code
vet:
    go vet ./...

# All-in-one linting
lint: fmt vet
    @echo 'Check for uncommitted changes ...'
    git diff --exit-code

# Build the docker image
build-docker: build
    docker build . --tag {{ GHCR_IMG }}

# Install the upstream COSI CRDs and central controller
install-cosi:
    kubectl apply -k config/crd
    kubectl -n container-object-storage-system rollout status \
        deployment/container-object-storage-controller --timeout=120s

# Apply the driver and its RBAC to the current cluster
install:
    kubectl apply -k config/manager

# Build, side-load into the athanor kind cluster and (re)deploy
deploy-kind: build-docker
    {{ KIND_CMD }} load docker-image {{ GHCR_IMG }} --name {{ KIND_CLUSTER }}
    kubectl apply -k config/manager
    kubectl -n {{ NAMESPACE }} rollout restart deployment/{{ bin_filename }}
    kubectl -n {{ NAMESPACE }} rollout status deployment/{{ bin_filename }} --timeout=120s

# Clean up the generated resources
clean:
    rm -rf dist/ cover.out {{ bin_filename }} || true
