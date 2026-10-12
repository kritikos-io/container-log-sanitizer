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
	"path/filepath"
	"strings"
	"testing"
	"time"

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
