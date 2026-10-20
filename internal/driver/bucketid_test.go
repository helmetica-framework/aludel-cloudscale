package driver

import "testing"

func TestBucketIDRoundTrip(t *testing.T) {
	want := BucketID{Region: "rma", Bucket: "bc-8f2a1c4e-1b3d-4e5f-9a7b-2c6d8e0f1a3b"}

	got, err := ParseBucketID(want.String())
	if err != nil {
		t.Fatalf("ParseBucketID(%q): %v", want.String(), err)
	}
	if got != want {
		t.Errorf("round trip gave %+v, want %+v", got, want)
	}
}

func TestParseBucketIDRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"",
		"rma",
		"rma/",
		"/my-bucket",
		// The v1alpha1 format carried the objects user ID as a third part.
		"rma/my-bucket/6fe39134bf41",
	} {
		if _, err := ParseBucketID(in); err == nil {
			t.Errorf("ParseBucketID(%q) = nil error, want failure", in)
		}
	}
}
