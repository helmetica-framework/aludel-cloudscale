package cloudscale

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmetica-framework/aludel-cloudscale/internal/driver"
)

// newTestClient points the SDK at a stub server via the CLOUDSCALE_API_URL
// override the SDK provides for exactly this purpose.
func newTestClient(t *testing.T, handler http.HandlerFunc) *ObjectsUsers {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("CLOUDSCALE_API_URL", srv.URL)

	users, err := New("test-token")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return users
}

// cloudscale.ch rejects a request carrying more than one `tag:` query clause
// with "At most one tag: clause can be passed". FindByTags must therefore push
// exactly one tag down and filter the remainder client-side.
func TestFindByTagsSendsSingleTagClause(t *testing.T) {
	var gotTagParams []string

	users := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		for key := range r.URL.Query() {
			if strings.HasPrefix(key, "tag:") {
				gotTagParams = append(gotTagParams, key)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})

	if _, err := users.FindByTags(context.Background(), map[string]string{
		"aludel_managed_by": "cloudscale-objectstorage.helmetica.io",
		"aludel_bucket":     "bucket-abc",
	}); err != nil {
		t.Fatalf("FindByTags: %v", err)
	}

	if len(gotTagParams) != 1 {
		t.Fatalf("sent %d tag: clauses (%v), cloudscale.ch allows at most 1", len(gotTagParams), gotTagParams)
	}
}

// The tags that could not be pushed down must still be honoured, otherwise
// FindByTags would return users belonging to other buckets.
func TestFindByTagsFiltersRemainingTagsClientSide(t *testing.T) {
	users := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// The server only applied the single pushed-down clause, so it returns
		// a user that matches that one but not the rest.
		body := []any{
			map[string]any{
				"id":           "user-match",
				"display_name": "bucket-abc",
				"tags": map[string]string{
					"aludel_managed_by": "cloudscale-objectstorage.helmetica.io",
					"aludel_bucket":     "bucket-abc",
				},
				"keys": []map[string]string{{"access_key": "AK", "secret_key": "SK"}},
			},
			map[string]any{
				"id":           "user-other-driver",
				"display_name": "bucket-abc",
				"tags": map[string]string{
					"aludel_managed_by": "some-other-driver",
					"aludel_bucket":     "bucket-abc",
				},
				"keys": []map[string]string{{"access_key": "AK2", "secret_key": "SK2"}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})

	got, err := users.FindByTags(context.Background(), map[string]string{
		"aludel_managed_by": "cloudscale-objectstorage.helmetica.io",
		"aludel_bucket":     "bucket-abc",
	})
	if err != nil {
		t.Fatalf("FindByTags: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d users, want 1 (client-side tag filtering did not apply)", len(got))
	}
	if got[0].ID != "user-match" {
		t.Errorf("got user %q, want %q", got[0].ID, "user-match")
	}
}

// A user's first key pair is what every access grant hands out.
func TestGetConvertsFirstKeyPair(t *testing.T) {
	users := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "6fe39134",
			"display_name": "alan",
			"keys": [{"access_key": "CLOUDSCALECHEXAMPLE", "secret_key": "TfJJUdA18Wo7EXAMPLEKEY"}],
			"tags": {}
		}`))
	})

	got, err := users.Get(context.Background(), "6fe39134")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AccessKey != "CLOUDSCALECHEXAMPLE" || got.SecretKey != "TfJJUdA18Wo7EXAMPLEKEY" {
		t.Errorf("keys not extracted: %+v", got)
	}
}

func TestGetMapsNotFound(t *testing.T) {
	users := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail": "Not found."}`))
	})

	_, err := users.Get(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !isDriverNotFound(err) {
		t.Errorf("error %v does not unwrap to driver.ErrNotFound", err)
	}
}

func isDriverNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), driver.ErrNotFound.Error())
}
