// Command aludel-cloudscale is a COSI driver for cloudscale.ch object storage.
//
// It provisions S3 buckets in response to Kubernetes BucketClaims and hands the
// consuming workload the credentials to reach them.
package main

import "github.com/helmetica-framework/aludel-cloudscale/cmd"

func main() {
	cmd.Execute()
}
