package driver

import (
	"fmt"
	"strings"
)

// BucketID is the opaque identifier aludel-cloudscale returns from DriverCreateBucket and
// receives back on every later call. COSI stores it verbatim on the Bucket
// resource, so it has to carry everything needed to act on the bucket later.
//
// Because a cloudscale.ch objects user can only reach the buckets it created
// itself, the owning user's ID is part of the identity of the bucket and is
// encoded here rather than looked up.
type BucketID struct {
	// Region is the cloudscale.ch region slug, e.g. "rma" or "lpg".
	Region string
	// Bucket is the S3 bucket name.
	Bucket string
	// UserID is the cloudscale.ch objects user that owns the bucket.
	UserID string
}

func (b BucketID) String() string {
	return strings.Join([]string{b.Region, b.Bucket, b.UserID}, "/")
}

// ParseBucketID reverses BucketID.String.
func ParseBucketID(s string) (BucketID, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return BucketID{}, fmt.Errorf("malformed bucket id %q: expected <region>/<bucket>/<objectsUserID>", s)
	}
	id := BucketID{Region: parts[0], Bucket: parts[1], UserID: parts[2]}
	if id.Region == "" || id.Bucket == "" || id.UserID == "" {
		return BucketID{}, fmt.Errorf("malformed bucket id %q: empty component", s)
	}
	return id, nil
}
