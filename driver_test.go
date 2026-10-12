package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/plugins/logdriver"
	"github.com/docker/docker/daemon/logger"
	"golang.org/x/sys/unix"
)

var testContainerID = strings.Repeat("a", 64)

func testStorageBackends(t *testing.T, check func(*testing.T, map[string]string)) {
	t.Helper()
	for _, name := range []string{"default", "local", "json-file"} {
		t.Run(name, func(t *testing.T) {
			config := make(map[string]string)
			if name != "default" {
				config["backend"] = name
			}
			check(t, config)
		})
	}
}

func newTestDriver(t *testing.T, root string) *driver {
	t.Helper()
	driver, err := newDriver(root, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { driver.Close() })
	return driver
}

func startTestStream(t *testing.T, driver *driver, containerID string, config map[string]string) (string, *os.File) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	if err := driver.StartLogging(path, logger.Info{ContainerID: containerID, Config: config}); err != nil {
		t.Fatal(err)
	}
	return path, writer
}

func TestDriverFIFOAndRestart(t *testing.T) {
	testStorageBackends(t, testDriverFIFOAndRestart)
}

func testDriverFIFOAndRestart(t *testing.T, config map[string]string) {
	t.Helper()
	root := t.TempDir()
	driver := newTestDriver(t, root)
	path, writer := startTestStream(t, driver, testContainerID, config)
	encoder := logdriver.NewLogEntryEncoder(writer)
	stamp := time.Now().UnixNano()
	for _, line := range []string{"\x00\x00\n", "\x1b[32mready\x1b[0m trace_id=0123456789abcdef0123456789abcdef", "keep\x00mixed"} {
		if err := encoder.Encode(&logdriver.LogEntry{Source: "stdout", Line: []byte(line), TimeNano: stamp}); err != nil {
			t.Fatal(err)
		}
	}
	writer.Close()
	if err := driver.StopLogging(path); err != nil {
		t.Fatal(err)
	}
	if driver.dropped.Load() != 1 || driver.stored.Load() != 2 {
		t.Fatalf("wrong persistence counts: dropped=%d stored=%d", driver.dropped.Load(), driver.stored.Load())
	}
	if err := driver.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := newTestDriver(t, root)
	store, active, err := restarted.openReader(testContainerID)
	if err != nil || active {
		t.Fatalf("could not read retained history: active=%v error=%v", active, err)
	}
	defer store.close()
	if store.storage.Name() != logBackendName(config) {
		t.Fatalf("reopened backend %q, want %q", store.storage.Name(), logBackendName(config))
	}
	messages := readRecords(t, store, logger.ReadConfig{Tail: -1})
	if len(messages) != 2 || !bytes.Contains(messages[0].Line, []byte("\x1b[32m")) || string(messages[1].Line) != "keep\x00mixed\n" {
		t.Fatalf("bad retained history: %#v", messages)
	}
}

func TestDriverReattachesAfterPluginRestart(t *testing.T) {
	testStorageBackends(t, testDriverReattachesAfterPluginRestart)
}

func testDriverReattachesAfterPluginRestart(t *testing.T, config map[string]string) {
	t.Helper()
	root := t.TempDir()
	driver := newTestDriver(t, root)
	path := filepath.Join(t.TempDir(), "docker-owned.fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := driver.StartLogging(path, logger.Info{ContainerID: testContainerID, Config: config}); err != nil {
		t.Fatal(err)
	}
	driver.Close()
	restarted := newTestDriver(t, root)
	if err := logdriver.NewLogEntryEncoder(writer).Encode(&logdriver.LogEntry{Source: "stdout", Line: []byte("after restart"), TimeNano: time.Now().UnixNano()}); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if err := restarted.StopLogging(path); err != nil {
		t.Fatal(err)
	}
	if restarted.stored.Load() != 1 {
		t.Fatal("restarted plugin did not consume the existing FIFO")
	}
	store, _, err := restarted.openReader(testContainerID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	messages := readRecords(t, store, logger.ReadConfig{Tail: -1})
	if len(messages) != 1 || string(messages[0].Line) != "after restart\n" {
		t.Fatalf("restart lost log: %#v", messages)
	}
}

func TestDriverRejectsBackendChange(t *testing.T) {
	testStorageBackends(t, func(t *testing.T, config map[string]string) {
		root := t.TempDir()
		driver := newTestDriver(t, root)
		path, writer := startTestStream(t, driver, testContainerID, config)
		if err := logdriver.NewLogEntryEncoder(writer).Encode(&logdriver.LogEntry{Source: "stdout", Line: []byte("retained"), TimeNano: time.Now().UnixNano()}); err != nil {
			t.Fatal(err)
		}
		if err := driver.StopLogging(path); err != nil {
			t.Fatal(err)
		}
		storePath, _ := logPath(root, testContainerID)
		before, err := os.ReadFile(storePath)
		if err != nil {
			t.Fatal(err)
		}
		other := "json-file"
		if logBackendName(config) == other {
			other = "local"
		}
		err = driver.StartLogging(path, logger.Info{ContainerID: testContainerID, Config: map[string]string{"backend": other}})
		if err == nil || !strings.Contains(err.Error(), "cannot change stored backend") {
			t.Fatalf("backend change was not rejected: %v", err)
		}
		after, err := os.ReadFile(storePath)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("backend change altered retained data: %v", err)
		}
		saved, err := loadOptions(root, testContainerID)
		if err != nil || logBackendName(saved) != logBackendName(config) {
			t.Fatalf("backend change altered saved options: %v (%v)", saved, err)
		}
	})
}

func TestDecodeEntryRejectsInvalidFrames(t *testing.T) {
	var oversized [4]byte
	binary.BigEndian.PutUint32(oversized[:], maxFrameSize+1)
	for _, payload := range [][]byte{oversized[:], {0, 0, 0, 0}, {0, 0, 0, 5, 1}, {0, 0, 0, 1, 0xff}, {1, 2}} {
		if _, err := decodeEntry(bytes.NewReader(payload)); err == nil {
			t.Fatalf("invalid frame accepted: %v", payload)
		}
	}
}

func TestDriverRejectsNonFIFOAndDuplicates(t *testing.T) {
	driver := newTestDriver(t, t.TempDir())
	regular := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(regular, []byte("not a FIFO"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := driver.StartLogging(regular, logger.Info{ContainerID: testContainerID}); err == nil {
		t.Fatal("accepted regular input file")
	}
	path, writer := startTestStream(t, driver, testContainerID, nil)
	if err := driver.StartLogging(path, logger.Info{ContainerID: testContainerID}); err == nil {
		t.Fatal("accepted duplicate stream")
	}
	writer.Close()
	if err := driver.StopLogging(path); err != nil {
		t.Fatal(err)
	}
	if err := driver.StopLogging(path); err != nil {
		t.Fatal("stop should be idempotent")
	}
}

func TestDriverMalformedStreamAndDrainDeadline(t *testing.T) {
	t.Run("malformed frame", func(t *testing.T) {
		driver := newTestDriver(t, t.TempDir())
		path, writer := startTestStream(t, driver, testContainerID, nil)
		if _, err := writer.Write([]byte{0xff, 0xff, 0xff, 0xff}); err != nil {
			t.Fatal(err)
		}
		writer.Close()
		if err := driver.StopLogging(path); err == nil || !strings.Contains(err.Error(), "frame size") {
			t.Fatalf("missing malformed-stream error: %v", err)
		}
		if driver.failed.Load() != 1 {
			t.Fatal("stream failure was not counted")
		}
	})
	t.Run("writer stays open", func(t *testing.T) {
		driver := newTestDriver(t, t.TempDir())
		path, writer := startTestStream(t, driver, testContainerID, nil)
		defer writer.Close()
		encoder := logdriver.NewLogEntryEncoder(writer)
		for index := 0; index < 100; index++ {
			if err := encoder.Encode(&logdriver.LogEntry{Source: "stdout", Line: []byte("drain me"), TimeNano: time.Now().UnixNano()}); err != nil {
				t.Fatal(err)
			}
		}
		started := time.Now()
		if err := driver.StopLogging(path); err != nil {
			t.Fatalf("Docker-style stop failed: %v", err)
		}
		if time.Since(started) > time.Second {
			t.Fatal("FIFO drain exceeded its bound")
		}
		if driver.stored.Load() != 100 {
			t.Fatal("stop discarded queued frames")
		}
	})
}

func TestStoreRotationAndReadOptions(t *testing.T) {
	testStorageBackends(t, testStoreRotationAndReadOptions)
}

func testStoreRotationAndReadOptions(t *testing.T, config map[string]string) {
	t.Helper()
	config["max-size"] = "512"
	config["max-file"] = "3"
	config["compress"] = "false"
	root := t.TempDir()
	store, err := newLogStore(root, logger.Info{ContainerID: testContainerID, Config: config})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.close() })
	stamp := time.Now().UTC().Add(-time.Hour)
	for index := 0; index < 40; index++ {
		line := fmt.Sprintf("line-%02d %s", index, strings.Repeat("x", 100))
		if _, err := store.write(&logdriver.LogEntry{Source: "stdout", Line: []byte(line), TimeNano: stamp.Add(time.Duration(index) * time.Second).UnixNano()}); err != nil {
			t.Fatal(err)
		}
	}
	path, _ := logPath(root, testContainerID)
	files, err := filepath.Glob(path + "*")
	if err != nil || len(files) != 3 {
		t.Fatalf("rotation did not retain exactly three files: %v (%v)", files, err)
	}
	messages := readRecords(t, store, logger.ReadConfig{Tail: 2})
	if len(messages) != 2 || !bytes.HasPrefix(messages[0].Line, []byte("line-38 ")) || !bytes.HasPrefix(messages[1].Line, []byte("line-39 ")) {
		t.Fatalf("tail mismatch: %#v", messages)
	}
	messages = readRecords(t, store, logger.ReadConfig{Tail: -1, Since: stamp.Add(38 * time.Second), Until: stamp.Add(38500 * time.Millisecond)})
	if len(messages) != 1 || !bytes.HasPrefix(messages[0].Line, []byte("line-38 ")) {
		t.Fatalf("time selection mismatch: %#v", messages)
	}
}

func readRecords(t *testing.T, store *logStore, config logger.ReadConfig) []*logger.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	watcher := store.read(ctx, config)
	defer watcher.ConsumerGone()
	var messages []*logger.Message
	for {
		select {
		case message, open := <-watcher.Msg:
			if !open {
				return messages
			}
			messages = append(messages, message)
		case err := <-watcher.Err:
			t.Fatalf("reading logs: %v", err)
		case <-ctx.Done():
			t.Fatal("log reader did not finish")
		}
	}
}

func TestStoreFiltersBeforePersistence(t *testing.T) {
	for _, backendName := range []string{"local", "json-file"} {
		t.Run(backendName, func(t *testing.T) {
			testStoreFiltersBeforePersistence(t, backendName)
		})
	}
}

func testStoreFiltersBeforePersistence(t *testing.T, backendName string) {
	t.Helper()
	root := t.TempDir()
	store, err := newLogStore(root, logger.Info{ContainerID: testContainerID, Config: map[string]string{"backend": backendName}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.close() })
	if store.storage.Name() != backendName {
		t.Fatalf("selected backend %q, want %q", store.storage.Name(), backendName)
	}
	path, _ := logPath(root, testContainerID)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for count := 0; count < 1000; count++ {
		kept, err := store.write(&logdriver.LogEntry{Source: "stdout", Line: []byte("\x00\x00 \t"), TimeNano: time.Now().UnixNano()})
		if err != nil || kept {
			t.Fatalf("placeholder persisted: kept=%v error=%v", kept, err)
		}
	}
	after, _ := os.Stat(path)
	if after.Size() != before.Size() {
		t.Fatalf("placeholders changed file size: %d -> %d", before.Size(), after.Size())
	}
	lines := []string{"\x1b[31mERROR\x1b[0m trace_id=0123456789abcdef0123456789abcdef span_id=0123456789abcdef", "before\x00after", "", " \t"}
	stamp := time.Now().UTC()
	for _, line := range lines {
		kept, err := store.write(&logdriver.LogEntry{Source: "stderr", Line: []byte(line), TimeNano: stamp.UnixNano()})
		if err != nil || !kept {
			t.Fatalf("meaningful record dropped: %v", err)
		}
	}
	messages := readRecords(t, store, logger.ReadConfig{Tail: -1})
	if len(messages) != len(lines) {
		t.Fatalf("got %d records, want %d", len(messages), len(lines))
	}
	for index, message := range messages {
		if string(message.Line) != lines[index]+"\n" || message.Source != "stderr" || !message.Timestamp.Equal(stamp) {
			t.Fatalf("record not preserved: %#v", message)
		}
	}
}

func TestStoreReopenAndPartial(t *testing.T) {
	testStorageBackends(t, testStoreReopenAndPartial)
}

func testStoreReopenAndPartial(t *testing.T, config map[string]string) {
	t.Helper()
	root := t.TempDir()
	info := logger.Info{ContainerID: testContainerID, Config: config}
	store, err := newLogStore(root, info)
	if err != nil {
		t.Fatal(err)
	}
	entry := &logdriver.LogEntry{Source: "stdout", TimeNano: time.Now().UnixNano(), Line: []byte("\x00\x00"), Partial: true,
		PartialLogMetadata: &logdriver.PartialLogEntryMetadata{Id: "split", Ordinal: 1, Last: false}}
	if kept, err := store.write(entry); err != nil || !kept {
		t.Fatalf("partial fragment must not be dropped: %v", err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	store, err = newLogStore(root, info)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.close() })
	messages := readRecords(t, store, logger.ReadConfig{Tail: -1})
	if len(messages) != 1 || !reflect.DeepEqual(messages[0].Line, entry.Line) {
		t.Fatalf("partial payload lost on reopen: %#v", messages)
	}
	if logBackendName(config) == "local" && (messages[0].PLogMetaData == nil || messages[0].PLogMetaData.ID != "split") {
		t.Fatalf("local backend lost partial metadata: %#v", messages)
	}
	if logBackendName(config) == "json-file" && messages[0].PLogMetaData != nil {
		t.Fatalf("unexpected metadata not stored by the JSON backend: %#v", messages)
	}
}

func TestStoreValidationAndRedaction(t *testing.T) {
	for _, config := range []map[string]string{{"backend": "unsupported"}, {"backend": ""}, {"sanitize-patterns": `["["]`}, {"drop-nul-placeholders": "maybe"}, {"unknown": "value"}, {"max-file": "0"}} {
		if store, err := newLogStore(t.TempDir(), logger.Info{ContainerID: testContainerID, Config: config}); err == nil {
			store.close()
			t.Fatalf("accepted invalid config: %v", config)
		}
	}
	if _, err := newLogStore(t.TempDir(), logger.Info{ContainerID: "../../elsewhere"}); err == nil {
		t.Fatal("accepted invalid container ID")
	}
	testStorageBackends(t, testStoreRedaction)
}

func testStoreRedaction(t *testing.T, config map[string]string) {
	t.Helper()
	config["sanitize-patterns"] = `["token=[^ ]+"]`
	store, err := newLogStore(t.TempDir(), logger.Info{ContainerID: testContainerID, Config: config})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.close() })
	if _, err := store.write(&logdriver.LogEntry{Source: "stdout", Line: []byte("token=private trace_id=keep"), TimeNano: time.Now().UnixNano()}); err != nil {
		t.Fatal(err)
	}
	messages := readRecords(t, store, logger.ReadConfig{Tail: -1})
	if len(messages) != 1 || string(messages[0].Line) != "*** trace_id=keep\n" {
		t.Fatalf("redaction failed: %#v", messages)
	}
}

func TestIsPlaceholder(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{name: "NUL", line: "\x00", want: true},
		{name: "NUL and whitespace", line: "\x00 \t\r\n\x00", want: true},
		{name: "empty", line: "", want: false},
		{name: "intentional blank", line: " \t\r\n", want: false},
		{name: "text", line: "request completed", want: false},
		{name: "mixed content", line: "before\x00after", want: false},
		{name: "ANSI color", line: "\x1b[31mERROR\x1b[0m", want: false},
		{name: "trace context", line: "trace_id=0123456789abcdef0123456789abcdef span_id=0123456789abcdef", want: false},
		{name: "non ASCII bytes", line: "\xff\x00", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isPlaceholder([]byte(test.line)); got != test.want {
				t.Fatalf("isPlaceholder(%q) = %v, want %v", test.line, got, test.want)
			}
		})
	}
}
