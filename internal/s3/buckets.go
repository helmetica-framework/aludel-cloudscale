// Package s3 talks to the cloudscale.ch S3-compatible endpoint (Ceph RGW).
//
// Every operation is performed with the credentials of the objects user that
// owns the bucket, so clients are built per call rather than shared.
//
// minio-go is used rather than the AWS SDK because it targets S3-compatible
// endpoints directly: no AWS endpoint resolution to work around, and none of
// the AWS-specific request checksums that S3-compatible servers reject. It is
// also what vshn/provider-cloudscale uses against the same storage.
package s3

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/helmetica-framework/aludel-cloudscale/internal/driver"
)

// S3 error codes cloudscale.ch's RGW returns that the driver acts on.
const (
	codeBucketAlreadyOwnedByYou = "BucketAlreadyOwnedByYou"
	codeBucketAlreadyExists     = "BucketAlreadyExists"
	codeNoSuchBucket            = "NoSuchBucket"
	codeBucketNotEmpty          = "BucketNotEmpty"
	codeAccessDenied            = "AccessDenied"
	codeInvalidAccessKeyID      = "InvalidAccessKeyId"
)

// Factory builds per-user S3 clients.
type Factory struct{}

var _ driver.BucketsFactory = (*Factory)(nil)

func (Factory) New(endpointURL, region, accessKey, secretKey string, pathStyle bool) (driver.Buckets, error) {
	if endpointURL == "" {
		return nil, errors.New("endpoint URL is empty")
	}

	// minio-go wants a bare host and a TLS flag, not a URL.
	u, err := url.Parse(endpointURL)
	if err != nil {
		return nil, fmt.Errorf("parsing endpoint URL %q: %w", endpointURL, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("endpoint URL %q has no host", endpointURL)
	}

	lookup := minio.BucketLookupDNS
	if pathStyle {
		lookup = minio.BucketLookupPath
	}

	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:       u.Scheme != "http",
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("building S3 client for %q: %w", endpointURL, err)
	}

	return &Buckets{client: client, region: region}, nil
}

// Buckets implements driver.Buckets for one objects user.
type Buckets struct {
	client *minio.Client
	region string
}

var _ driver.Buckets = (*Buckets)(nil)

func (b *Buckets) Create(ctx context.Context, name string) error {
	err := b.client.MakeBucket(ctx, name, minio.MakeBucketOptions{Region: b.region})
	if err == nil {
		return nil
	}

	switch errorCode(err) {
	case codeBucketAlreadyOwnedByYou:
		// This very user already created it, which is exactly what a retried
		// DriverCreateBucket should tolerate.
		return nil
	case codeBucketAlreadyExists, codeAccessDenied:
		// RGW answers AccessDenied, not BucketAlreadyExists, when the name is
		// held by an account we cannot see.
		return driver.ErrBucketNameExists
	case codeInvalidAccessKeyID:
		// The key pair has not propagated to this region's RGW cluster yet.
		return fmt.Errorf("MakeBucket: %w: %v", driver.ErrCredentialsNotReady, err)
	}
	return fmt.Errorf("MakeBucket: %w", err)
}

func (b *Buckets) Delete(ctx context.Context, name string) error {
	err := b.client.RemoveBucket(ctx, name)
	if err == nil {
		return nil
	}

	switch errorCode(err) {
	case codeNoSuchBucket:
		return driver.ErrNotFound
	case codeBucketNotEmpty:
		return driver.ErrBucketNotEmpty
	}
	return fmt.Errorf("RemoveBucket: %w", err)
}

// Empty deletes every object version in the bucket.
//
// minio-go streams the listing into RemoveObjects, so paging and batching are
// handled by the library; only the first failure is reported back.
func (b *Buckets) Empty(ctx context.Context, name string) error {
	objects := b.client.ListObjects(ctx, name, minio.ListObjectsOptions{
		Recursive:    true,
		WithVersions: true,
	})

	// ListObjects reports its own failures through ObjectInfo.Err, which
	// RemoveObjects would otherwise swallow, so they are pulled out here and
	// reported over a channel of their own.
	filtered := make(chan minio.ObjectInfo)
	listErrCh := make(chan error, 1)

	go func() {
		defer close(listErrCh)
		defer close(filtered)

		for obj := range objects {
			if obj.Err != nil {
				listErrCh <- obj.Err
				return
			}
			select {
			case filtered <- obj:
			case <-ctx.Done():
				return
			}
		}
	}()

	for removeErr := range b.client.RemoveObjects(ctx, name, filtered, minio.RemoveObjectsOptions{}) {
		if removeErr.Err != nil {
			return fmt.Errorf("removing %q: %w", removeErr.ObjectName, removeErr.Err)
		}
	}

	// Closed without a send yields nil, so this covers both outcomes.
	if listErr := <-listErrCh; listErr != nil {
		if errorCode(listErr) == codeNoSuchBucket {
			return driver.ErrNotFound
		}
		return fmt.Errorf("listing objects: %w", listErr)
	}
	return nil
}

// errorCode extracts the S3 error code from a minio-go error, if any.
func errorCode(err error) string {
	return minio.ToErrorResponse(err).Code
}
