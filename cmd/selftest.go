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
	Long: `selftest exercises DriverGenerateBucketId, DriverCreateBucket,
DriverGetBucket, DriverGrantBucketAccess and DriverDeleteBucket end to end
without Kubernetes.

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
		// "bc-<uuid>", like the names the COSI controller generates. Bucket
		// names are unique across all of cloudscale.ch, so the name has to
		// carry no meaning; anything left behind is identified by the
		// objects user's tags instead.
		name := "bc-" + uuid.NewString()
		params := map[string]string{
			driver.ParamRegion:         flagSelftestRegion,
			driver.ParamDeletionPolicy: string(driver.DeleteAll),
			driver.ParamPathStyle:      fmt.Sprint(flagSelftestPathStyle),
		}
		if flagSelftestEndpoint != "" {
			params[driver.ParamEndpointURL] = flagSelftestEndpoint
		}

		s3 := []*cosi.ObjectProtocol{{Type: cosi.ObjectProtocol_S3}}

		step(1, "DriverGenerateBucketId", "name=%s region=%s", name, flagSelftestRegion)
		generated, err := p.DriverGenerateBucketId(ctx, &cosi.DriverGenerateBucketIdRequest{
			Name:       name,
			Protocols:  s3,
			Parameters: params,
		})
		if err != nil {
			return fmt.Errorf("DriverGenerateBucketId failed: %w", err)
		}
		bucketID := generated.GetBucketId()
		fmt.Printf("    ok  bucketID=%s\n", bucketID)

		step(2, "DriverCreateBucket", "bucketID=%s endpoint=%s pathStyle=%t",
			bucketID, endpointOf(params), flagSelftestPathStyle)
		if _, err := p.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{
			BucketId:   bucketID,
			Protocols:  s3,
			Parameters: params,
		}); err != nil {
			return fmt.Errorf("DriverCreateBucket failed: %w", err)
		}
		fmt.Printf("    ok\n")

		step(3, "DriverGetBucket", "bucketID=%s", bucketID)
		got, err := p.DriverGetBucket(ctx, &cosi.DriverGetBucketRequest{
			BucketId:   bucketID,
			Protocols:  s3,
			Parameters: params,
		})
		if err != nil {
			return fmt.Errorf("DriverGetBucket failed: %w", err)
		}
		fmt.Printf("    ok  bucket=%s endpoint=%s\n",
			got.GetProtocols().GetS3().GetBucketId(), got.GetProtocols().GetS3().GetEndpoint())

		step(4, "DriverGrantBucketAccess", "authenticationType=Key accessModes=ReadWrite")
		readWrite := &cosi.AccessMode{Mode: cosi.AccessMode_READ_WRITE}
		granted, err := p.DriverGrantBucketAccess(ctx, &cosi.DriverGrantBucketAccessRequest{
			AccountName:        "ba-selftest",
			Protocol:           s3[0],
			AuthenticationType: &cosi.AuthenticationType{Type: cosi.AuthenticationType_KEY},
			Parameters:         params,
			Buckets: []*cosi.DriverGrantBucketAccessRequest_AccessedBucket{{
				BucketId:                 bucketID,
				ObjectDataAccessMode:     readWrite,
				ObjectMetadataAccessMode: readWrite,
				BucketMetadataAccessMode: readWrite,
			}},
		})
		if err != nil {
			return fmt.Errorf("DriverGrantBucketAccess failed: %w", err)
		}
		info := granted.GetBuckets()[0].GetBucketInfo().GetS3()
		fmt.Printf("    ok  accountID=%s accessKeyID=%s endpoint=%s region=%s\n",
			granted.GetAccountId(), granted.GetCredentials().GetS3().GetAccessKeyId(),
			info.GetEndpoint(), info.GetRegion())

		if flagSelftestKeep {
			fmt.Printf("\n--keep given: leaving bucket %q and its objects user in place.\n", name)
			fmt.Printf("Delete them yourself, or re-run without --keep.\n")
			return nil
		}

		step(5, "DriverDeleteBucket", "bucketDeletionPolicy=DeleteAll")
		if _, err := p.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{
			BucketId:   bucketID,
			Parameters: params,
		}); err != nil {
			return fmt.Errorf("DriverDeleteBucket failed: %w", err)
		}
		fmt.Printf("    ok  bucket and objects user removed\n")

		fmt.Printf("\nAll five RPCs succeeded. The cloudscale.ch and S3 paths work;\n")
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
