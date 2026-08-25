package cmd

import (
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	cosi "sigs.k8s.io/container-object-storage-interface/proto"

	"github.com/helmetica-framework/aludel-cloudscale/internal/cloudscale"
	"github.com/helmetica-framework/aludel-cloudscale/internal/driver"
	"github.com/helmetica-framework/aludel-cloudscale/internal/s3"
)

var (
	flagSelftestRegion    string
	flagSelftestEndpoint  string
	flagSelftestPathStyle bool
	flagSelftestKeep      bool
)

// selftestCmd runs a full provision/grant/delete cycle against the real
// cloudscale.ch API using the same Provisioner the sidecar drives, but without
// Kubernetes in the picture.
//
// It exists to split a failure in two: if selftest passes, the cloudscale.ch
// and S3 code paths are fine and the problem is in the COSI wiring (driver
// name, sidecar, RBAC, CRDs). If it fails, the failing call is named directly.
var selftestCmd = &cobra.Command{
	Use:   "selftest",
	Short: "Provision, grant and delete a throwaway bucket against the real API",
	Long: `selftest exercises DriverCreateBucket, DriverGrantBucketAccess and
DriverDeleteBucket end to end without Kubernetes.

It creates a REAL objects user and a REAL bucket in your cloudscale.ch project
and deletes both again unless --keep is given.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		log, err := newLogger(flagLogLevel)
		if err != nil {
			return err
		}

		token := os.Getenv(tokenEnvVar)
		if token == "" {
			return fmt.Errorf("%s is not set", tokenEnvVar)
		}

		users, err := cloudscale.New(token)
		if err != nil {
			return err
		}

		p := &driver.Provisioner{
			Name:    flagDriverName,
			Users:   users,
			Buckets: s3.Factory{},
			Log:     log,
		}

		ctx := cmd.Context()
		// A bare UUID, like the names the COSI controller generates. Bucket
		// names are unique across all of cloudscale.ch, so the name has to
		// carry no meaning; anything left behind is identified by the
		// objects user's tags instead.
		name := uuid.NewString()
		params := map[string]string{
			driver.ParamRegion:         flagSelftestRegion,
			driver.ParamDeletionPolicy: string(driver.DeleteAll),
			driver.ParamPathStyle:      fmt.Sprint(flagSelftestPathStyle),
		}
		if flagSelftestEndpoint != "" {
			params[driver.ParamEndpointURL] = flagSelftestEndpoint
		}

		step(1, "DriverCreateBucket", "name=%s region=%s endpoint=%s pathStyle=%t",
			name, flagSelftestRegion, endpointOf(params), flagSelftestPathStyle)
		created, err := p.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{
			Name:       name,
			Parameters: params,
		})
		if err != nil {
			return fmt.Errorf("DriverCreateBucket failed: %w", err)
		}
		fmt.Printf("    ok  bucketID=%s\n", created.GetBucketId())

		step(2, "DriverGrantBucketAccess", "authenticationType=Key")
		granted, err := p.DriverGrantBucketAccess(ctx, &cosi.DriverGrantBucketAccessRequest{
			BucketId:           created.GetBucketId(),
			Name:               "selftest-access",
			AuthenticationType: cosi.AuthenticationType_Key,
		})
		if err != nil {
			return fmt.Errorf("DriverGrantBucketAccess failed: %w", err)
		}
		secrets := granted.GetCredentials()["s3"].GetSecrets()
		fmt.Printf("    ok  accountID=%s accessKeyID=%s endpoint=%s region=%s\n",
			granted.GetAccountId(), secrets["accessKeyID"], secrets["endpoint"], secrets["region"])

		if flagSelftestKeep {
			fmt.Printf("\n--keep given: leaving bucket %q and its objects user in place.\n", name)
			fmt.Printf("Delete them yourself, or re-run without --keep.\n")
			return nil
		}

		step(3, "DriverDeleteBucket", "bucketDeletionPolicy=DeleteAll")
		if _, err := p.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{
			BucketId:      created.GetBucketId(),
			DeleteContext: params,
		}); err != nil {
			return fmt.Errorf("DriverDeleteBucket failed: %w", err)
		}
		fmt.Printf("    ok  bucket and objects user removed\n")

		fmt.Printf("\nAll three RPCs succeeded. The cloudscale.ch and S3 paths work;\n")
		fmt.Printf("if the driver still misbehaves in-cluster the problem is in the COSI wiring.\n")
		return nil
	},
}

func step(n int, rpc, format string, args ...any) {
	fmt.Printf("\n[%d] %s\n    %s\n", n, rpc, fmt.Sprintf(format, args...))
}

func endpointOf(params map[string]string) string {
	if e := params[driver.ParamEndpointURL]; e != "" {
		return e
	}
	return driver.DefaultEndpointURL(params[driver.ParamRegion])
}

func init() {
	selftestCmd.Flags().StringVar(&flagSelftestRegion, "region", "rma",
		"cloudscale.ch region slug")
	selftestCmd.Flags().StringVar(&flagSelftestEndpoint, "endpoint-url", "",
		"S3 endpoint override; defaults to https://objects.<region>.cloudscale.ch")
	selftestCmd.Flags().BoolVar(&flagSelftestPathStyle, "path-style", true,
		"use path-style addressing instead of virtual-hosted")
	selftestCmd.Flags().BoolVar(&flagSelftestKeep, "keep", false,
		"do not delete the bucket and objects user afterwards")
	selftestCmd.Flags().StringVar(&flagLogLevel, "log-level", "debug",
		"log level: debug, info, warn or error")
	selftestCmd.Flags().StringVar(&flagDriverName, "driver-name", defaultDriverName,
		"driver name used for the objects user tags")
}
