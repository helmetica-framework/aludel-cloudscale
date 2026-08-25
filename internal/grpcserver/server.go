// Package grpcserver hosts the COSI gRPC services on a unix socket shared with
// the COSI sidecar.
package grpcserver

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"google.golang.org/grpc"
	cosi "sigs.k8s.io/container-object-storage-interface/proto"
)

// Serve listens on endpoint (a unix:// URL) and serves the COSI Identity and
// Provisioner services until ctx is cancelled.
func Serve(ctx context.Context, endpoint string, impl interface {
	cosi.IdentityServer
	cosi.ProvisionerServer
}, log *slog.Logger) error {
	lis, err := listen(endpoint)
	if err != nil {
		return err
	}

	srv := grpc.NewServer()
	cosi.RegisterIdentityServer(srv, impl)
	cosi.RegisterProvisionerServer(srv, impl)

	go func() {
		<-ctx.Done()
		log.Info("shutting down gRPC server")
		srv.GracefulStop()
	}()

	log.Info("serving COSI", "endpoint", endpoint)
	return srv.Serve(lis)
}

// listen resolves the COSI endpoint URL and prepares the socket. A stale socket
// file from a previous crash is removed, otherwise bind fails with EADDRINUSE.
func listen(endpoint string) (net.Listener, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parsing endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "unix" {
		return nil, fmt.Errorf("unsupported endpoint scheme %q, only unix:// is supported", u.Scheme)
	}

	path := u.Path
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating socket directory: %w", err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale socket %q: %w", path, err)
	}

	return net.Listen("unix", path)
}
