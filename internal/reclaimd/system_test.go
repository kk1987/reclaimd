package reclaimd

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// os-release values come double-quoted, single-quoted or bare, and a rolling
// distribution writes no version at all. The first file that exists wins.
func TestOSPrettyNameReadsOSRelease(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	openwrt := write("openwrt", "NAME=\"OpenWrt\"\nVERSION_ID=\"25.12.5\"\nPRETTY_NAME=\"OpenWrt 25.12.5\"\n")
	bare := write("bare", "# no PRETTY_NAME here\nNAME=Alpine\nVERSION_ID=3.22.1\n")
	arch := write("arch", "NAME='Arch Linux'\nBUILD_ID=rolling\n")
	missing := filepath.Join(dir, "missing")

	for _, c := range []struct {
		paths []string
		want  string
	}{
		{[]string{openwrt}, "OpenWrt 25.12.5"},
		{[]string{bare}, "Alpine 3.22.1"},
		{[]string{missing, arch}, "Arch Linux"},
		{[]string{missing}, ""},
	} {
		if got := osPrettyName(c.paths...); got != c.want {
			t.Errorf("%v: got %q, want %q", c.paths, got, c.want)
		}
	}
}

// The page asks the daemon which build and which machine it is, and gets the
// build the command handed over along with what the machine says of itself.
func TestSystemEndpointNamesTheBuildAndTheMachine(t *testing.T) {
	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	s := NewServer(mustConfig(t), store, &Supervisor{store: store, logger: quietLogger()}, quietLogger())
	s.Build = BuildInfo{Version: "v9.9.9", Commit: "abc1234", Date: "2026-09-11T00:00:00Z"}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/system", nil))
	var got SystemInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%d %s: %v", rec.Code, rec.Body, err)
	}
	if got.BuildInfo != s.Build {
		t.Errorf("build %+v, want %+v", got.BuildInfo, s.Build)
	}
	host, _ := os.Hostname()
	if got.Hostname != host || got.Arch != runtime.GOARCH || got.Kernel == "" {
		t.Errorf("machine: host %q, arch %q, kernel %q", got.Hostname, got.Arch, got.Kernel)
	}
}
