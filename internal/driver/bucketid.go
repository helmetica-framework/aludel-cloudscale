package driver

import (
	"fmt"
	"strings"
)

// BucketID is the opaque identifier aludel-cloudscale returns from
// DriverGenerateBucketId and receives back on every later call. COSI stores it
// verbatim on the Bucket resource, so it has to carry everything needed to act
// on the bucket later.
//
// COSI v1alpha2 provisions in two phases and persists the ID before
// DriverCreateBucket runs, so the ID cannot contain anything that only exists
// once the bucket does. In particular the owning objects user's ID is not
// part of it: the user is found again through the tags set on it at creation.
type BucketID struct {
	// Region is the cloudscale.ch region slug, e.g. "rma" or "lpg".
	Region string
	// Bucket is the S3 bucket name.
	Bucket string
}

func (b BucketID) String() string {
	return b.Region + "/" + b.Bucket
}

// ParseBucketID reverses BucketID.String.
func ParseBucketID(s string) (BucketID, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return BucketID{}, fmt.Errorf("malformed bucket id %q: expected <region>/<bucket>", s)
	}
	id := BucketID{Region: parts[0], Bucket: parts[1]}
	if id.Region == "" || id.Bucket == "" {
		return BucketID{}, fmt.Errorf("malformed bucket id %q: empty component", s)
	}
	return id, nil
}
