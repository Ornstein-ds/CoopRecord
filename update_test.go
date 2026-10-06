package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type updateTestTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func TestUpdateGitHubSmoke(t *testing.T) {
	if os.Getenv("COOPRECORD_UPDATE_SMOKE") != "1" {
		t.Skip("set COOPRECORD_UPDATE_SMOKE=1 to download and verify the public release")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	path, tag, err := downloadUpdate(ctx, &http.Client{Timeout: 2 * time.Minute}, releaseAPI, "0.0.0", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	found := false
	for _, entry := range archive.File {
		if entry.Name == "CoopRecord.exe" {
			found = true
		}
	}
	if !found {
		t.Fatal("CoopRecord.exe missing from downloaded ZIP")
	}
	t.Logf("Downloaded and verified public release %s without credentials", tag)
}

func (rt updateTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	copy.URL.Scheme, copy.URL.Host = rt.target.Scheme, rt.target.Host
	return rt.base.RoundTrip(copy)
}

func TestUpdateDownload(t *testing.T) {
	archive := "test release archive"
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(archive)))
	for _, tc := range []struct {
		name, tag, current, digest, assetURL string
		status, size                         int
		wantDownload, wantError              bool
	}{
		{name: "newer", tag: "v1.0.2", wantDownload: true},
		{name: "same", tag: "v1.0.1"},
		{name: "older", tag: "v1.0.0"},
		{name: "numeric comparison", tag: "v1.0.10", current: "1.0.9", wantDownload: true},
		{name: "major", tag: "v2.0.0", current: "1.9.9", wantDownload: true},
		{name: "bad checksum", tag: "v1.0.2", digest: "sha256:" + strings.Repeat("0", 64), wantError: true},
		{name: "missing checksum", tag: "v1.0.2", digest: "missing", wantError: true},
		{name: "truncated", tag: "v1.0.2", size: len(archive) + 1, wantError: true},
		{name: "too large", tag: "v1.0.2", size: maxUpdateSize + 1, wantError: true},
		{name: "foreign URL", tag: "v1.0.2", assetURL: "https://example.com/update.zip", wantError: true},
		{name: "invalid tag", tag: "../../escape", wantError: true},
		{name: "prerelease tag", tag: "v1.0.2-beta", wantError: true},
		{name: "not found", status: 404, wantError: true},
		{name: "rate limit", status: 403, wantError: true},
		{name: "server error", status: 500, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.current == "" {
				tc.current = "1.0.1"
			}
			if tc.digest == "" {
				tc.digest = digest
			}
			if tc.size == 0 {
				tc.size = len(archive)
			}
			if tc.assetURL == "" {
				tc.assetURL = "https://github.com/Ornstein-ds/CoopRecord/releases/download/v1.0.2/" + updateArchive
			}
			assetRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/latest" {
					if tc.status != 0 {
						w.WriteHeader(tc.status)
						return
					}
					fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q,"digest":%q,"size":%d}]}`, tc.tag, updateArchive, tc.assetURL, tc.digest, tc.size)
					return
				}
				assetRequests++
				fmt.Fprint(w, archive)
			}))
			defer server.Close()
			target, _ := url.Parse(server.URL)
			client := &http.Client{Transport: updateTestTransport{target, http.DefaultTransport}}
			dir := t.TempDir()
			// A failed retry must preserve an already downloaded archive.
			existing := filepath.Join(dir, "v1.0.2", updateArchive)
			if err := os.MkdirAll(filepath.Dir(existing), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(existing, []byte("previous download"), 0600); err != nil {
				t.Fatal(err)
			}
			path, tag, err := downloadUpdate(context.Background(), client, server.URL+"/latest", tc.current, dir)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v", err)
			}
			if tc.wantDownload {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != archive || tag != tc.tag || assetRequests != 1 {
					t.Fatalf("download: %q, %q, %v", data, tag, err)
				}
				// Repeated download can replace an existing verified ZIP on Windows.
				if _, _, err := downloadUpdate(context.Background(), client, server.URL+"/latest", tc.current, dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if path != "" {
					t.Fatalf("unexpected archive: %s", path)
				}
				data, _ := os.ReadFile(existing)
				if string(data) != "previous download" {
					t.Fatal("previous download was changed")
				}
				if !tc.wantError && assetRequests != 0 {
					t.Fatal("unnecessary download")
				}
			}
			_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(path, ".partial") {
					t.Error("partial download left behind")
				}
				return nil
			})
		})
	}
}
