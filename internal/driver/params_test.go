package driver

import "testing"

func TestParseBucketClassParametersDefaults(t *testing.T) {
	p, err := ParseBucketClassParameters(map[string]string{ParamRegion: "lpg"})
	if err != nil {
		t.Fatalf("ParseBucketClassParameters: %v", err)
	}

	if p.EndpointURL != "https://objects.lpg.cloudscale.ch" {
		t.Errorf("endpoint = %q, want the derived cloudscale.ch endpoint", p.EndpointURL)
	}
	if p.DeletionPolicy != DeleteIfEmpty {
		t.Errorf("deletion policy = %q, want %q", p.DeletionPolicy, DeleteIfEmpty)
	}
	if !p.PathStyle {
		t.Error("path style should default to true")
	}
}

func TestParseBucketClassParametersOverrides(t *testing.T) {
	p, err := ParseBucketClassParameters(map[string]string{
		ParamRegion:         "rma",
		ParamEndpointURL:    "https://objects.example.test",
		ParamDeletionPolicy: string(DeleteAll),
		ParamPathStyle:      "false",
	})
	if err != nil {
		t.Fatalf("ParseBucketClassParameters: %v", err)
	}

	if p.EndpointURL != "https://objects.example.test" {
		t.Errorf("endpoint = %q", p.EndpointURL)
	}
	if p.DeletionPolicy != DeleteAll {
		t.Errorf("deletion policy = %q", p.DeletionPolicy)
	}
	if p.PathStyle {
		t.Error("path style should be false")
	}
}

func TestParseBucketClassParametersErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"missing region":          {},
		"unknown deletion policy": {ParamRegion: "rma", ParamDeletionPolicy: "Nuke"},
		"non-boolean path style":  {ParamRegion: "rma", ParamPathStyle: "yes-please"},
	}

	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBucketClassParameters(params); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}
