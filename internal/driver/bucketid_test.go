package driver

import "testing"

func TestBucketIDRoundTrip(t *testing.T) {
	want := BucketID{Region: "rma", Bucket: "my-bucket", UserID: "6fe39134bf41"}

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
		"rma/my-bucket",
		"rma/my-bucket/user/extra",
		"rma//user",
		"/my-bucket/user",
		"rma/my-bucket/",
	} {
		if _, err := ParseBucketID(in); err == nil {
			t.Errorf("ParseBucketID(%q) = nil error, want failure", in)
		}
	}
}
