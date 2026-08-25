package cmd

import (
	"errors"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/helmetica-framework/aludel-cloudscale/internal/cloudscale"
	"github.com/helmetica-framework/aludel-cloudscale/internal/driver"
	"github.com/helmetica-framework/aludel-cloudscale/internal/grpcserver"
	"github.com/helmetica-framework/aludel-cloudscale/internal/s3"
)

const (
	// tokenEnvVar holds the cloudscale.ch API token. It must be read-write:
	// read-only tokens omit objects user secret keys, which aludel-cloudscale needs on
	// every DriverGrantBucketAccess.
	tokenEnvVar = "CLOUDSCALE_API_TOKEN"

	defaultEndpoint   = "unix:///var/lib/cosi/cosi.sock"
	defaultDriverName = "cloudscale-objectstorage.helmetica.io"
)

var (
	flagEndpoint   string
	flagDriverName string
	flagLogLevel   string
)

var driverCmd = &cobra.Command{
	Use:   "driver",
	Short: "Run the COSI provisioner gRPC server",
	RunE: func(cmd *cobra.Command, _ []string) error {
		log, err := newLogger(flagLogLevel)
		if err != nil {
			return err
		}

		token := os.Getenv(tokenEnvVar)
		if token == "" {
			return errors.New(tokenEnvVar + " is not set")
		}

		users, err := cloudscale.New(token)
		if err != nil {
			return err
		}

		provisioner := &driver.Provisioner{
			Name:    flagDriverName,
			Users:   users,
			Buckets: s3.Factory{},
			Log:     log,
		}

		log.Info("starting aludel-cloudscale", "driver", flagDriverName)
		return grpcserver.Serve(cmd.Context(), flagEndpoint, provisioner, log)
	},
}

func init() {
	driverCmd.Flags().StringVar(&flagEndpoint, "endpoint", defaultEndpoint,
		"unix:// socket the COSI sidecar connects to")
	driverCmd.Flags().StringVar(&flagDriverName, "driver-name", defaultDriverName,
		"driver name reported by DriverGetInfo; must match the BucketClass driverName")
	driverCmd.Flags().StringVar(&flagLogLevel, "log-level", "info",
		"log level: debug, info, warn or error")
}

func newLogger(level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, err
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})), nil
}
