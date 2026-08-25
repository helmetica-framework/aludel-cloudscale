package driver

import (
	"fmt"
	"strconv"
)

// Parameter keys understood on a BucketClass. The sidecar copies BucketClass
// parameters onto the Bucket resource and hands them back as delete_context on
// DriverDeleteBucket, so the same struct parses both.
const (
	ParamRegion         = "region"
	ParamEndpointURL    = "endpointURL"
	ParamDeletionPolicy = "bucketDeletionPolicy"
	ParamPathStyle      = "s3PathStyle"
)

// DeletionPolicy decides what DriverDeleteBucket does with a non-empty bucket.
type DeletionPolicy string

const (
	// DeleteIfEmpty refuses to delete a bucket that still holds objects.
	DeleteIfEmpty DeletionPolicy = "DeleteIfEmpty"
	// DeleteAll removes every object (and version) before dropping the bucket.
	DeleteAll DeletionPolicy = "DeleteAll"
)

// BucketClassParameters is the parsed, validated form of a BucketClass's
// parameters map.
type BucketClassParameters struct {
	Region         string
	EndpointURL    string
	DeletionPolicy DeletionPolicy
	PathStyle      bool
}

// ParseBucketClassParameters validates the opaque parameter map COSI passes
// through from the BucketClass.
func ParseBucketClassParameters(params map[string]string) (*BucketClassParameters, error) {
	p := &BucketClassParameters{
		DeletionPolicy: DeleteIfEmpty,
		PathStyle:      true,
	}

	p.Region = params[ParamRegion]
	if p.Region == "" {
		return nil, fmt.Errorf("parameter %q is required", ParamRegion)
	}

	p.EndpointURL = params[ParamEndpointURL]
	if p.EndpointURL == "" {
		p.EndpointURL = DefaultEndpointURL(p.Region)
	}

	switch v := DeletionPolicy(params[ParamDeletionPolicy]); v {
	case "":
		// keep the default
	case DeleteIfEmpty, DeleteAll:
		p.DeletionPolicy = v
	default:
		return nil, fmt.Errorf("parameter %q: unknown value %q, expected %q or %q",
			ParamDeletionPolicy, v, DeleteIfEmpty, DeleteAll)
	}

	if v := params[ParamPathStyle]; v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", ParamPathStyle, err)
		}
		p.PathStyle = b
	}

	return p, nil
}

// DefaultEndpointURL returns the public cloudscale.ch objects endpoint for a
// region.
func DefaultEndpointURL(region string) string {
	return fmt.Sprintf("https://objects.%s.cloudscale.ch", region)
}
