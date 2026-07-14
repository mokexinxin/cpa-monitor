package cliproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVersionStatusReadsRunningHeaderAndLatestRelease(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/prefix/v0/management/latest-version" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+testManagementKey; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		w.Header().Set("X-CPA-VERSION", "v7.2.70")
		_, _ = io.WriteString(w, `{"latest-version":"v7.2.74"}`)
	}))
	defer server.Close()

	status, err := mustClient(t, server.URL+"/prefix", time.Second).VersionStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.CurrentVersion != "v7.2.70" || status.LatestVersion != "v7.2.74" || !status.ComparisonAvailable || !status.UpdateAvailable {
		t.Fatalf("VersionStatus() = %#v", status)
	}
}

func TestCompareReleaseVersions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		current, latest string
		want            int
		comparable      bool
	}{
		{current: "v7.2.74", latest: "7.2.74", want: 0, comparable: true},
		{current: "v7.2.73", latest: "v7.2.74", want: -1, comparable: true},
		{current: "v7.3.0", latest: "v7.2.74", want: 1, comparable: true},
		{current: "v7.2.74-rc.1", latest: "v7.2.74", want: -1, comparable: true},
		{current: "v7.2.74", latest: "v7.2.75-rc.1", want: -1, comparable: true},
		{current: "dev", latest: "v7.2.74", comparable: false},
	}
	for _, tt := range tests {
		got, comparable := compareReleaseVersions(tt.current, tt.latest)
		if comparable != tt.comparable || got != tt.want {
			t.Errorf("compareReleaseVersions(%q, %q) = %d, %v; want %d, %v", tt.current, tt.latest, got, comparable, tt.want, tt.comparable)
		}
	}
}

func TestVersionStatusRejectsUnavailableOrInvalidData(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		status  int
		current string
		body    string
	}{
		{name: "management failure", status: http.StatusBadGateway, current: "v7.2.70", body: `{}`},
		{name: "missing current", status: http.StatusOK, body: `{"latest-version":"v7.2.74"}`},
		{name: "missing latest", status: http.StatusOK, current: "v7.2.70", body: `{}`},
		{name: "malformed JSON", status: http.StatusOK, current: "v7.2.70", body: `{"latest-version":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.current != "" {
					w.Header().Set("X-CPA-VERSION", tt.current)
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			if _, err := mustClient(t, server.URL, time.Second).VersionStatus(context.Background()); err == nil {
				t.Fatal("VersionStatus() error = nil")
			}
		})
	}
}
