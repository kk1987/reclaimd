package reclaimd

import (
	"bufio"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// BuildInfo is what the binary was built from. Only the command knows it -- it
// is linked into main -- so the command hands it to the server.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"build_date"`
}

// SystemInfo is the build and the machine a status page is talking to. The
// page for a router and the page for a laptop are otherwise the same page, and
// a tab left open across an upgrade gives no sign of which daemon answers it.
type SystemInfo struct {
	BuildInfo
	Hostname string `json:"hostname"`
	// OS is the distribution as os-release names it, "OpenWrt 25.12.5" or
	// "Arch Linux", and empty on a system without the file.
	OS string `json:"os"`
	// Kernel is the kernel's name and release, "Linux 6.12.94".
	Kernel string `json:"kernel"`
	// Arch is what the binary was built for, which is also which release
	// asset is running.
	Arch string `json:"arch"`
}

func readSystemInfo(build BuildInfo) SystemInfo {
	host, _ := os.Hostname()
	name, release := kernelVersion()
	return SystemInfo{
		BuildInfo: build,
		Hostname:  host,
		OS:        osPrettyName("/etc/os-release", "/usr/lib/os-release"),
		Kernel:    strings.TrimSpace(name + " " + release),
		Arch:      runtime.GOARCH,
	}
}

// handleSystem answers which build and which machine the page is talking to.
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, readSystemInfo(s.Build))
}

// osPrettyName reads the first os-release file that exists, in the order the
// format's specification gives. PRETTY_NAME is meant for exactly this; without
// it, NAME and VERSION_ID say the same. A rolling distribution such as Arch has
// no version to give, and its name alone is the right answer.
func osPrettyName(paths ...string) string {
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		vars := map[string]string{}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			k, v, ok := strings.Cut(line, "=")
			if !ok || strings.HasPrefix(line, "#") {
				continue
			}
			vars[k] = unquoteOSRelease(v)
		}
		f.Close()
		if s := vars["PRETTY_NAME"]; s != "" {
			return s
		}
		return strings.TrimSpace(vars["NAME"] + " " + vars["VERSION_ID"])
	}
	return ""
}

// unquoteOSRelease undoes the shell-style quoting an os-release value may carry.
func unquoteOSRelease(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		if s, err := strconv.Unquote(v); err == nil {
			return s
		}
		return v[1 : len(v)-1]
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	return v
}

// firstLine is the first line of a small file, or "" when it cannot be read.
func firstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}
