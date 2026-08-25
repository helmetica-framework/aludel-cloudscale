// Package driver implements the COSI Identity and Provisioner services for
// cloudscale.ch object storage.
//
// It depends only on the ObjectsUsers and Buckets interfaces declared here, so
// the provisioning logic can be tested without touching either the cloudscale.ch
// API or an S3 endpoint. The adapters live in internal/cloudscale and internal/s3.
package driver

import (
	"context"
	"errors"
)

// ErrNotFound is returned when the remote object does not exist.
var ErrNotFound = errors.New("not found")

// ErrBucketNameExists is returned when the bucket name already exists as the
// bucket name has to be unique across all buckets globally.
var ErrBucketNameExists = errors.New("bucket name already exists")

// ErrBucketNotEmpty is returned when deleting a non-empty bucket under the
// DeleteIfEmpty policy.
var ErrBucketNotEmpty = errors.New("bucket not empty")

// ErrCredentialsNotReady is returned when the S3 endpoint does not (yet)
// recognise the access key.
//
// A newly created objects user key pair takes a moment to propagate from
// the cloudscale.ch API to the RGW cluster serving the region, so the first
// S3 call after Create can legitimately fail this way. We should therefore retry the call.
var ErrCredentialsNotReady = errors.New("credentials not recognised by the S3 endpoint")

// ObjectsUser is the subset of a cloudscale.ch objects user aludel-cloudscale cares about.
//
// AccessKey/SecretKey come from the user's first key pair. cloudscale.ch
// returns the secret on every GET (provided the API token is read-write), so
// the driver never has to persist credentials itself.
type ObjectsUser struct {
	ID          string
	DisplayName string
	AccessKey   string
	SecretKey   string
	Tags        map[string]string
}

// ObjectsUsers is the cloudscale.ch REST API surface aludel-cloudscale needs.
type ObjectsUsers interface {
	Create(ctx context.Context, displayName string, tags map[string]string) (*ObjectsUser, error)
	Get(ctx context.Context, id string) (*ObjectsUser, error)
	// FindByTags does a server-side tag-filtered list. Used to make
	// DriverCreateBucket idempotent across retries.
	FindByTags(ctx context.Context, tags map[string]string) ([]ObjectsUser, error)
	Delete(ctx context.Context, id string) error
}

// Buckets is the S3 surface aludel-cloudscale needs, scoped to one set of credentials.
type Buckets interface {
	Create(ctx context.Context, name string) error
	// Empty removes all objects (including non-current versions) from a bucket.
	Empty(ctx context.Context, name string) error
	// Delete removes the bucket. It returns ErrBucketNotEmpty if objects remain
	// and ErrNotFound if the bucket is already gone.
	Delete(ctx context.Context, name string) error
}

// BucketsFactory builds an S3 client for a specific objects user. Every bucket
// is created and deleted by its own owning user, so there is no long-lived S3
// client in the driver.
type BucketsFactory interface {
	New(endpointURL, region, accessKey, secretKey string, pathStyle bool) (Buckets, error)
}
