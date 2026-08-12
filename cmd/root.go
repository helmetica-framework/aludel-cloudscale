// Package cmd holds the command line entry points: driver, which serves COSI
// to the sidecar, and selftest, which exercises the same code against the real
// cloudscale.ch API without Kubernetes.
package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "aludel-cloudscale",
	Short: "A COSI driver for cloudscale.ch object storage",
	Long: `aludel-cloudscale provisions cloudscale.ch S3 buckets through the Kubernetes
Container Object Storage Interface (COSI).

cloudscale.ch offers no IAM, roles or bucket policies, so aludel-cloudscale runs one
objects user per bucket: the user is created together with the bucket and
deleted with it, and every BucketAccess hands out that user's key pair.`,
}

func init() {
	rootCmd.AddCommand(driverCmd)
	rootCmd.AddCommand(selftestCmd)
}

// Execute runs the root command with a context cancelled on SIGINT/SIGTERM.
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
