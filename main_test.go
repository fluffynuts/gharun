package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestUpgradeInstallsLatestRelease(t *testing.T) {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	exe := "gharun"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	asset := fmt.Sprintf("gharun-%s-%s.zip", osName, runtime.GOARCH)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("gharun-0.1.9-" + osName + "-" + runtime.GOARCH + "/" + exe)
	w.Write([]byte("new binary"))
	zw.Close()
	sum := sha256.Sum256(buf.Bytes())

	serve := func(sums string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/" + asset:
				rw.Write(buf.Bytes())
			case "/SHA256SUMS":
				fmt.Fprint(rw, sums)
			default:
				http.NotFound(rw, r)
			}
		}))
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	bad := serve(fmt.Sprintf("%064x  %s\n", 0, asset))
	defer bad.Close()
	t.Setenv("GHARUN_RELEASE_URL", bad.URL)
	if code := upgrade(); code == 0 {
		t.Fatal("upgrade accepted a checksum mismatch")
	}

	good := serve(fmt.Sprintf("%x  %s\n", sum, asset))
	defer good.Close()
	t.Setenv("GHARUN_RELEASE_URL", good.URL)
	if code := upgrade(); code != 0 {
		t.Fatalf("upgrade exited %d", code)
	}
	got, err := os.ReadFile(filepath.Join(home, ".local", "bin", exe))
	if err != nil || string(got) != "new binary" {
		t.Fatalf("installed binary = %q, %v", got, err)
	}
}
