package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/plugins/logdriver"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/pkg/plugins"
	"golang.org/x/sys/unix"
)

func TestPluginSocketPath(t *testing.T) {
	data, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Interface struct{ Socket string }
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Interface.Socket != filepath.Base(defaultSocketPath) {
		t.Fatal("manifest socket differs from executable default")
	}
	hostPath := filepath.Join(filepath.Dir(defaultSocketPath), strings.Repeat("a", 64), manifest.Interface.Socket)
	limit := len(unix.RawSockaddrUnix{}.Path) - 1
	if len(hostPath) > limit {
		t.Fatalf("Docker plugin socket path is %d bytes, limit %d: %s", len(hostPath), limit, hostPath)
	}
}

func TestPluginMetadata(test *testing.T) {
	data, err := os.ReadFile("plugin.json")
	if err != nil {
		test.Fatal(err)
	}
	var config types.PluginConfig
	if err := json.Unmarshal(data, &config); err != nil {
		test.Fatal(err)
	}
	if config.Description == "" {
		test.Fatal("plugin description is missing")
	}
	if config.Documentation != "https://github.com/kritikos-io/container-log-sanitizer/blob/main/README.md" {
		test.Fatalf("unexpected plugin documentation link: %q", config.Documentation)
	}
}

func TestPluginInstallerKeepsPortableConfig(test *testing.T) {
	installer, err := os.ReadFile("build-plugin.sh")
	if err != nil {
		test.Fatal(err)
	}
	remaining := string(installer)
	for _, command := range []string{
		`install -D -m 0644 README.md "$staging/rootfs/usr/share/doc/container-log-sanitizer/README.md"`,
		`install -D -m 0644 LICENSE "$staging/rootfs/usr/share/doc/container-log-sanitizer/LICENSE"`,
		`cp plugin.json "$staging/config.json"`,
		`docker plugin create "$plugin_name" "$staging"`,
		`docker plugin set "$plugin_name" "logdata.source=$data_root"`,
		`docker plugin enable --timeout 0 "$plugin_name"`,
	} {
		position := strings.Index(remaining, command)
		if position == -1 {
			test.Fatalf("installer must run %q in order", command)
		}
		remaining = remaining[position+len(command):]
	}
}

func TestPublisherFailureKeepsParentShellAlive(test *testing.T) {
	command := exec.Command("bash", "-c", `
set +e +u
set +o pipefail
bash publish-plugin.sh unexpected-argument
status=$?
printf 'parent shell alive; child exit=%s\n' "$status"
test "$status" -eq 1
`)
	output, err := command.CombinedOutput()
	if err != nil {
		test.Fatalf("parent shell exited unexpectedly: %v\n%s", err, output)
	}
	for _, expected := range []string{"Usage: bash publish-plugin.sh", "parent shell alive; child exit=1"} {
		if !strings.Contains(string(output), expected) {
			test.Fatalf("missing %q in shell output: %s", expected, output)
		}
	}
}

func TestPublisherPackagesWithoutHostInstallation(test *testing.T) {
	publisher, err := os.ReadFile("publish-plugin.sh")
	if err != nil {
		test.Fatal(err)
	}
	manifest, err := os.ReadFile("plugin.json")
	if err != nil {
		test.Fatal(err)
	}
	var pluginMetadata types.PluginConfig
	if err := json.Unmarshal(manifest, &pluginMetadata); err != nil {
		test.Fatal(err)
	}
	files := map[string][]byte{"publish-plugin.sh": publisher, "plugin.json": manifest}
	files["registry-manifest.json"] = []byte(`{
	"schemaVersion": 2,
	"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
	"config": {
		"mediaType": "application/vnd.docker.plugin.v1+json",
		"size": 2,
		"digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	},
	"layers": [{
		"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
		"size": 0,
		"digest": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	}],
	"annotations": {"example.org/preserved": "yes"}
}`)
	for _, name := range []string{"README.md", "LICENSE"} {
		contents, err := os.ReadFile(name)
		if err != nil {
			test.Fatal(err)
		}
		files[name] = contents
	}
	for _, testCase := range []struct {
		name             string
		testStatus       string
		pushStatus       string
		annotationStatus string
		wantStatus       int
	}{
		{name: "success", testStatus: "0", pushStatus: "0", annotationStatus: "0"},
		{name: "tests fail", testStatus: "23", pushStatus: "0", annotationStatus: "0", wantStatus: 23},
		{name: "push fails", testStatus: "0", pushStatus: "24", annotationStatus: "0", wantStatus: 24},
		{name: "annotations fail", testStatus: "0", pushStatus: "0", annotationStatus: "25", wantStatus: 25},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			root := test.TempDir()
			for name, contents := range files {
				if err := os.WriteFile(filepath.Join(root, name), contents, 0600); err != nil {
					test.Fatal(err)
				}
			}
			command := exec.Command("bash", "-c", `
git() {
	printf '%s\n' "$*" >> git-calls
	return 90
}
dotnet-gitversion() {
	[[ "$*" == '. -config GitVersion.yml -nofetch -nocache -showvariable SemVer' ]] || return 97
	printf '1.2.3\n'
}
docker() {
  printf '%s\n' "$*" >> docker-calls
  case "$1 $2" in
		'login containers.kritikos.io') ;;
    'plugin create')
      cmp plugin.json "$4/config.json" || return 91
      cmp dist/container-log-sanitizer "$4/rootfs/container-log-sanitizer" || return 92
      test -x "$4/rootfs/container-log-sanitizer" || return 93
			cmp README.md "$4/rootfs/usr/share/doc/container-log-sanitizer/README.md" || return 98
			cmp LICENSE "$4/rootfs/usr/share/doc/container-log-sanitizer/LICENSE" || return 99
      ;;
    'plugin push') return "$PUBLISH_PUSH_STATUS" ;;
    *) printf 'Unexpected Docker operation: %s\n' "$*" >&2; return 94 ;;
  esac
}
go() {
  case "$1" in
    test) return "$PUBLISH_TEST_STATUS" ;;
    build) mkdir -p dist; printf 'test binary\n' > dist/container-log-sanitizer ;;
    *) return 95 ;;
  esac
}
regctl() {
	printf '%s\n' "$*" >> registry-calls
	case "$*" in
		'manifest get containers.kritikos.io/system/container-log-sanitizer:1.2.3 --format raw-body')
			cat registry-manifest.json
			;;
		'manifest put containers.kritikos.io/system/container-log-sanitizer:1.2.3 --content-type application/vnd.docker.distribution.manifest.v2+json')
			cat > registry-manifest-annotated.json
			return "$PUBLISH_ANNOTATION_STATUS"
			;;
		*) printf 'Unexpected registry operation: %s\n' "$*" >&2; return 100 ;;
	esac
}
sudo() { printf 'Publisher must not elevate privileges\n' >&2; return 96; }
export -f git dotnet-gitversion docker go regctl sudo
bash publish-plugin.sh
`)
			command.Dir = root
			command.Env = append(os.Environ(), "PUBLISH_TEST_STATUS="+testCase.testStatus,
				"PUBLISH_PUSH_STATUS="+testCase.pushStatus, "PUBLISH_ANNOTATION_STATUS="+testCase.annotationStatus)
			output, err := command.CombinedOutput()
			if command.ProcessState == nil || command.ProcessState.ExitCode() != testCase.wantStatus {
				test.Fatalf("publisher exit did not match %d: %v\n%s", testCase.wantStatus, err, output)
			}
			if _, err := os.Stat(filepath.Join(root, "git-calls")); !os.IsNotExist(err) {
				test.Fatalf("publisher must not run Git commands: %v", err)
			}
			calls, err := os.ReadFile(filepath.Join(root, "docker-calls"))
			if err != nil && !os.IsNotExist(err) {
				test.Fatal(err)
			}
			if testCase.testStatus != "0" && len(calls) != 0 {
				test.Fatalf("failed tests must prevent registry and plugin operations: %s", calls)
			}
			pushed := strings.Contains(string(calls), "plugin push containers.kritikos.io/system/container-log-sanitizer:1.2.3")
			if pushed != (testCase.testStatus == "0") {
				test.Fatalf("push must only follow successful tests: %s", calls)
			}
			registryCalls, err := os.ReadFile(filepath.Join(root, "registry-calls"))
			if err != nil && !os.IsNotExist(err) {
				test.Fatal(err)
			}
			shouldAnnotate := testCase.testStatus == "0" && testCase.pushStatus == "0"
			if (len(registryCalls) != 0) != shouldAnnotate {
				test.Fatalf("annotations must only follow a successful plugin push: %s", registryCalls)
			}
			if shouldAnnotate {
				data, err := os.ReadFile(filepath.Join(root, "registry-manifest-annotated.json"))
				if err != nil {
					test.Fatal(err)
				}
				var before, after map[string]any
				if err := json.Unmarshal(files["registry-manifest.json"], &before); err != nil {
					test.Fatal(err)
				}
				if err := json.Unmarshal(data, &after); err != nil {
					test.Fatal(err)
				}
				annotations, ok := after["annotations"].(map[string]any)
				if !ok {
					test.Fatal("registry manifest annotations are missing")
				}
				for key, want := range map[string]string{
					"example.org/preserved":                  "yes",
					"org.opencontainers.image.title":         "Container Log Sanitizer",
					"org.opencontainers.image.description":   pluginMetadata.Description,
					"org.opencontainers.image.documentation": pluginMetadata.Documentation,
					"org.opencontainers.image.source":        "https://github.com/kritikos-io/container-log-sanitizer",
					"org.opencontainers.image.licenses":      "Apache-2.0",
					"org.opencontainers.image.version":       "1.2.3",
				} {
					if annotations[key] != want {
						test.Errorf("annotation %s = %v, want %q", key, annotations[key], want)
					}
				}
				created, _ := annotations["org.opencontainers.image.created"].(string)
				if _, err := time.Parse(time.RFC3339, created); err != nil {
					test.Fatalf("invalid build timestamp %q: %v", created, err)
				}
				delete(before, "annotations")
				delete(after, "annotations")
				beforeJSON, _ := json.Marshal(before)
				afterJSON, _ := json.Marshal(after)
				if !bytes.Equal(beforeJSON, afterJSON) {
					test.Fatal("annotation step changed plugin manifest content or references")
				}
			}
			staging, err := filepath.Glob(filepath.Join(root, ".plugin-build.*"))
			if err != nil || len(staging) != 0 {
				test.Fatalf("temporary packaging files remain: %v (%v)", staging, err)
			}
		})
	}
}

func rpc(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data)))
	return response
}

func requireRPCSuccess(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	var result pluginResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Err != "" {
		t.Fatalf("RPC failed: %s (%v)", response.Body.String(), err)
	}
}

func TestRPCProtocolAndPersistence(t *testing.T) {
	testStorageBackends(t, testRPCProtocolAndPersistence)
}

func testRPCProtocolAndPersistence(t *testing.T, config map[string]string) {
	t.Helper()
	driver := newTestDriver(t, t.TempDir())
	handler := newHandler(driver)
	response := rpc(t, handler, "/LogDriver.Capabilities", struct{}{})
	var capabilities struct{ Cap logger.Capability }
	if err := json.Unmarshal(response.Body.Bytes(), &capabilities); err != nil || !capabilities.Cap.ReadLogs {
		t.Fatalf("incorrect capability response: %s", response.Body.String())
	}
	path := filepath.Join(t.TempDir(), "logs.fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	info := logger.Info{ContainerID: testContainerID, Config: config}
	writer, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	requireRPCSuccess(t, rpc(t, handler, "/LogDriver.StartLogging", struct {
		File string
		Info logger.Info
	}{path, info}))
	stamp := time.Now().UTC()
	encoder := logdriver.NewLogEntryEncoder(writer)
	entries := []*logdriver.LogEntry{
		{Source: "stderr", Line: []byte("\x00\x00"), TimeNano: stamp.UnixNano()},
		{Source: "stderr", Line: []byte("\x1b[31mERROR\x1b[0m trace_id=0123456789abcdef0123456789abcdef span_id=0123456789abcdef"), TimeNano: stamp.UnixNano()},
	}
	for _, entry := range entries {
		if err := encoder.Encode(entry); err != nil {
			t.Fatal(err)
		}
	}
	requireRPCSuccess(t, rpc(t, handler, "/LogDriver.StopLogging", struct{ File string }{path}))
	writer.Close()
	response = rpc(t, handler, "/LogDriver.ReadLogs", struct {
		Info   logger.Info
		Config logger.ReadConfig
	}{info, logger.ReadConfig{Tail: -1}})
	if response.Code != http.StatusOK {
		t.Fatalf("read request failed: %s", response.Body.String())
	}
	entry, err := decodeEntry(response.Body)
	if err != nil || string(entry.Line) != string(entries[1].Line)+"\n" || entry.Source != "stderr" || entry.TimeNano != stamp.UnixNano() {
		t.Fatalf("wire record changed: %#v (%v)", entry, err)
	}
	if _, err := decodeEntry(response.Body); !errors.Is(err, io.EOF) {
		t.Fatalf("expected one retained record, got %v", err)
	}
}

func TestRPCFollowAndCancel(t *testing.T) {
	testStorageBackends(t, testRPCFollowAndCancel)
}

func testRPCFollowAndCancel(t *testing.T, config map[string]string) {
	t.Helper()
	driver := newTestDriver(t, t.TempDir())
	path, writer := startTestStream(t, driver, testContainerID, config)
	server := httptest.NewServer(newHandler(driver))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	data, _ := json.Marshal(struct {
		Info   logger.Info
		Config logger.ReadConfig
	}{logger.Info{ContainerID: testContainerID}, logger.ReadConfig{Tail: 0, Follow: true}})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/LogDriver.ReadLogs", bytes.NewReader(data))
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := logdriver.NewLogEntryEncoder(writer).Encode(&logdriver.LogEntry{Source: "stdout", Line: []byte("live"), TimeNano: time.Now().UnixNano()}); err != nil {
		t.Fatal(err)
	}
	entry, err := decodeEntry(response.Body)
	if err != nil || string(entry.Line) != "live\n" {
		t.Fatalf("follow did not deliver: %#v (%v)", entry, err)
	}
	cancel()
	response.Body.Close()
	writer.Close()
	if err := driver.StopLogging(path); err != nil {
		t.Fatal(err)
	}
}

func TestRPCFollowWithPluginClientTimeout(t *testing.T) {
	installer, err := os.ReadFile("build-plugin.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installer), "docker plugin enable --timeout 0 \"$plugin_name\"") {
		t.Fatal("installer must disable the total HTTP deadline for ReadLogs streams")
	}
	testStorageBackends(t, func(t *testing.T, config map[string]string) {
		for _, clientTimeout := range []time.Duration{100 * time.Millisecond, 0} {
			t.Run(clientTimeout.String(), func(t *testing.T) {
				driver := newTestDriver(t, t.TempDir())
				path, writer := startTestStream(t, driver, testContainerID, config)
				server := httptest.NewServer(newHandler(driver))
				defer server.Close()
				client, err := plugins.NewClientWithTimeout(server.URL, nil, clientTimeout)
				if err != nil {
					t.Fatal(err)
				}
				stream, err := client.Stream("LogDriver.ReadLogs", struct {
					Info   logger.Info
					Config logger.ReadConfig
				}{logger.Info{ContainerID: testContainerID}, logger.ReadConfig{Tail: 0, Follow: true}})
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				deadline := time.AfterFunc(3*time.Second, func() { stream.Close() })
				defer deadline.Stop()
				writeResult := make(chan error, 1)
				delivery := time.AfterFunc(250*time.Millisecond, func() {
					writeResult <- logdriver.NewLogEntryEncoder(writer).Encode(&logdriver.LogEntry{
						Source: "stdout", Line: []byte("late trace_id=0123456789abcdef0123456789abcdef"), TimeNano: time.Now().UnixNano(),
					})
				})
				defer delivery.Stop()
				entry, err := decodeEntry(stream)
				if clientTimeout > 0 {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("expected Docker client deadline, got entry=%v error=%v", entry, err)
					}
					return
				}
				if err != nil || string(entry.Line) != "late trace_id=0123456789abcdef0123456789abcdef\n" {
					t.Fatalf("unbounded plugin client failed to follow: entry=%v error=%v", entry, err)
				}
				if err := <-writeResult; err != nil {
					t.Fatal(err)
				}
				stream.Close()
				writer.Close()
				if err := driver.StopLogging(path); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
}

func TestRPCRejectsMalformedRequests(t *testing.T) {
	handler := newHandler(newTestDriver(t, t.TempDir()))
	for _, payload := range []string{"{", "{} {}", "{}"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/LogDriver.StartLogging", bytes.NewBufferString(payload)))
		var result pluginResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Err == "" {
			t.Fatalf("malformed request accepted: %s", payload)
		}
	}
}
