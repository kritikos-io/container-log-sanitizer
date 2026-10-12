package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/plugins/logdriver"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/docker/docker/daemon/logger/local"
	"github.com/kritikos-io/container-log-sanitizer/redactor"
	"golang.org/x/sys/unix"
)

const maxFrameSize = 1_000_000

var containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type driver struct {
	root        string
	logger      *log.Logger
	mu          sync.Mutex
	streams     map[string]*logStream
	active      map[string]*logStream
	closed      bool
	stopTimeout time.Duration
	stored      atomic.Uint64
	dropped     atomic.Uint64
	failed      atomic.Uint64
}

type logStream struct {
	path        string
	containerID string
	store       *logStore
	input       *drainableFIFO
	done        chan struct{}
	err         error
}

type drainableFIFO struct {
	file     *os.File
	stopping atomic.Bool
}

func (input *drainableFIFO) Read(buffer []byte) (int, error) {
	for {
		if err := input.file.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			return 0, err
		}
		count, err := input.file.Read(buffer)
		if errors.Is(err, os.ErrDeadlineExceeded) && count == 0 {
			if input.stopping.Load() {
				return 0, io.EOF
			}
			continue
		}
		return count, err
	}
}

func (input *drainableFIFO) Close() error {
	return input.file.Close()
}

func newDriver(root string, diagnostics *log.Logger) (*driver, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("log root must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	driver := &driver{root: root, logger: diagnostics, streams: make(map[string]*logStream), active: make(map[string]*logStream), stopTimeout: 5 * time.Second}
	registrations, err := filepath.Glob(filepath.Join(root, "*", "stream.json"))
	if err != nil {
		return nil, err
	}
	for _, registration := range registrations {
		var saved struct{ FIFO string }
		data, err := os.ReadFile(registration)
		if err == nil {
			err = json.Unmarshal(data, &saved)
		}
		if err != nil {
			driver.Close()
			return nil, fmt.Errorf("read stream registration: %w", err)
		}
		if _, err := os.Lstat(saved.FIFO); os.IsNotExist(err) {
			if err := os.Remove(registration); err != nil {
				driver.Close()
				return nil, err
			}
			continue
		}
		containerID := filepath.Base(filepath.Dir(registration))
		options, err := loadOptions(root, containerID)
		if err == nil {
			err = driver.StartLogging(saved.FIFO, logger.Info{ContainerID: containerID, Config: options})
		}
		if err != nil {
			driver.Close()
			return nil, fmt.Errorf("restore container %s: %w", containerID, err)
		}
	}
	return driver, nil
}

func (driver *driver) StartLogging(path string, info logger.Info) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.closed {
		return errors.New("driver is shutting down")
	}
	if _, exists := driver.streams[path]; exists {
		return errors.New("FIFO is already registered")
	}
	if _, exists := driver.active[info.ContainerID]; exists {
		return errors.New("container is already logging")
	}
	if !filepath.IsAbs(path) {
		return errors.New("FIFO path must be absolute")
	}
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fileInfo.Mode()&os.ModeNamedPipe == 0 {
		return errors.New("log input must be a FIFO, not a regular file or symlink")
	}
	store, err := newLogStore(driver.root, info)
	if err != nil {
		return err
	}
	if err := saveOptions(driver.root, info.ContainerID, info.Config); err != nil {
		store.close()
		return err
	}
	storePath, _ := logPath(driver.root, info.ContainerID)
	if err := saveJSON(filepath.Join(filepath.Dir(storePath), "stream.json"), struct{ FIFO string }{path}); err != nil {
		store.close()
		return err
	}
	file, err := os.OpenFile(path, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		store.close()
		return err
	}
	stream := &logStream{path: path, containerID: info.ContainerID, store: store, input: &drainableFIFO{file: file}, done: make(chan struct{})}
	driver.streams[path] = stream
	driver.active[info.ContainerID] = stream
	go driver.consume(stream)
	return nil
}

func decodeEntry(reader io.Reader) (*logdriver.LogEntry, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxFrameSize {
		return nil, fmt.Errorf("invalid frame size %d", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	entry := new(logdriver.LogEntry)
	if err := entry.Unmarshal(payload); err != nil {
		return nil, fmt.Errorf("invalid protobuf log record: %w", err)
	}
	return entry, nil
}

func (driver *driver) consume(stream *logStream) {
	defer func() {
		stream.input.Close()
		stream.err = errors.Join(stream.err, stream.store.close())
		driver.mu.Lock()
		delete(driver.active, stream.containerID)
		if !driver.closed {
			path, _ := logPath(driver.root, stream.containerID)
			if err := os.Remove(filepath.Join(filepath.Dir(path), "stream.json")); err != nil && !os.IsNotExist(err) {
				stream.err = errors.Join(stream.err, err)
			}
		}
		driver.mu.Unlock()
		if stream.err != nil {
			driver.failed.Add(1)
			driver.logger.Printf("container %s: log stream failed: %v", stream.containerID, stream.err)
		}
		close(stream.done)
	}()
	for {
		entry, err := decodeEntry(stream.input)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				stream.err = err
			}
			return
		}
		kept, err := stream.store.write(entry)
		if err != nil {
			stream.err = err
			return
		}
		if kept {
			driver.stored.Add(1)
		} else {
			driver.dropped.Add(1)
		}
	}
}

func (driver *driver) StopLogging(path string) error {
	driver.mu.Lock()
	stream := driver.streams[path]
	driver.mu.Unlock()
	if stream == nil {
		return nil
	}
	stream.input.stopping.Store(true)
	timer := time.NewTimer(driver.stopTimeout)
	defer timer.Stop()
	var stopErr error
	select {
	case <-stream.done:
	case <-timer.C:
		stopErr = errors.New("timed out draining FIFO; remaining input may be lost")
		stream.input.Close()
		<-stream.done
	}
	driver.mu.Lock()
	if driver.streams[path] == stream {
		delete(driver.streams, path)
	}
	driver.mu.Unlock()
	return errors.Join(stopErr, stream.err)
}

func (driver *driver) openReader(containerID string) (*logStore, bool, error) {
	path, err := logPath(driver.root, containerID)
	if err != nil {
		return nil, false, err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.closed {
		return nil, false, errors.New("driver is shutting down")
	}
	if stream := driver.active[containerID]; stream != nil {
		return stream.store, true, nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, false, err
	}
	options, err := loadOptions(driver.root, containerID)
	if err != nil {
		return nil, false, err
	}
	store, err := newLogStore(driver.root, logger.Info{ContainerID: containerID, Config: options})
	return store, false, err
}

func saveOptions(root, containerID string, options map[string]string) error {
	path, err := logPath(root, containerID)
	if err != nil {
		return err
	}
	return saveJSON(filepath.Join(filepath.Dir(path), "options.json"), options)
}

func loadOptions(root, containerID string) (map[string]string, error) {
	path, err := logPath(root, containerID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), "options.json"))
	if err != nil {
		return nil, err
	}
	var options map[string]string
	if err := json.Unmarshal(data, &options); err != nil {
		return nil, err
	}
	return options, nil
}

func saveJSON(path string, value any) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := json.NewEncoder(file).Encode(value); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (driver *driver) Close() error {
	driver.mu.Lock()
	driver.closed = true
	streams := make([]*logStream, 0, len(driver.streams))
	for _, stream := range driver.streams {
		streams = append(streams, stream)
	}
	driver.mu.Unlock()
	for _, stream := range streams {
		stream.input.Close()
	}
	var combined error
	for _, stream := range streams {
		<-stream.done
		combined = errors.Join(combined, stream.err)
	}
	return combined
}

type logStore struct {
	storage          logger.Logger
	reader           logger.LogReader
	redactor         *redactor.Redactor
	dropPlaceholders bool
}

func logPath(root, containerID string) (string, error) {
	if !containerIDPattern.MatchString(containerID) {
		return "", fmt.Errorf("container ID must be 64 lowercase hex characters")
	}
	return filepath.Join(root, containerID, "container.log"), nil
}

func logBackendName(options map[string]string) string {
	if name, configured := options["backend"]; configured {
		return name
	}
	return local.Name
}

func newLogStore(root string, info logger.Info) (*logStore, error) {
	path, err := logPath(root, info.ContainerID)
	if err != nil {
		return nil, err
	}
	storageName := logBackendName(info.Config)
	createStorage := local.New
	switch storageName {
	case local.Name:
	case jsonfilelog.Name:
		createStorage = jsonfilelog.New
	default:
		return nil, fmt.Errorf("unsupported backend %q: expected local or json-file", storageName)
	}
	if savedOptions, err := loadOptions(root, info.ContainerID); err == nil {
		if savedName := logBackendName(savedOptions); savedName != storageName {
			return nil, fmt.Errorf("cannot change stored backend from %q to %q for the same container; recreate the container", savedName, storageName)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read stored logging options: %w", err)
	}
	storageOptions := map[string]string{"max-size": "10m", "max-file": "3", "compress": "true"}
	dropPlaceholders := true
	var patterns []string
	for key, value := range info.Config {
		switch key {
		case "backend":
		case "max-size", "max-file", "compress":
			storageOptions[key] = value
		case "drop-nul-placeholders":
			dropPlaceholders, err = strconv.ParseBool(value)
			if err != nil {
				return nil, fmt.Errorf("invalid drop-nul-placeholders: %w", err)
			}
		case "sanitize-patterns":
			if err := json.Unmarshal([]byte(value), &patterns); err != nil {
				return nil, fmt.Errorf("sanitize-patterns must be a JSON array of regex strings: %w", err)
			}
		case "mode", "max-buffer-size", "cache-disabled", "cache-max-size", "cache-max-file", "cache-compress":
		default:
			return nil, fmt.Errorf("unknown logging option %q", key)
		}
	}
	sanitizer, err := redactor.New(patterns)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	storage, err := createStorage(logger.Info{ContainerID: info.ContainerID, LogPath: path, Config: storageOptions})
	if err != nil {
		return nil, err
	}
	return &logStore{storage: storage, reader: storage.(logger.LogReader), redactor: sanitizer, dropPlaceholders: dropPlaceholders}, nil
}

func (store *logStore) write(entry *logdriver.LogEntry) (bool, error) {
	partial := entry.Partial || entry.PartialLogMetadata != nil
	if store.dropPlaceholders && !partial && isPlaceholder(entry.Line) {
		return false, nil
	}
	if entry.Source != "stdout" && entry.Source != "stderr" {
		return false, fmt.Errorf("invalid log stream %q", entry.Source)
	}
	message := logger.NewMessage()
	message.Source = entry.Source
	message.Timestamp = time.Unix(0, entry.TimeNano).UTC()
	message.Line = append(message.Line, entry.Line...)
	if !partial {
		message.Line = store.redactor.RedactBytes(message.Line)
	}
	if metadata := entry.PartialLogMetadata; metadata != nil {
		message.PLogMetaData = &backend.PartialLogMetaData{ID: metadata.Id, Ordinal: int(metadata.Ordinal), Last: metadata.Last}
	} else if entry.Partial {
		logger.PutMessage(message)
		return false, fmt.Errorf("partial record is missing metadata")
	}
	if err := store.storage.Log(message); err != nil {
		return false, err
	}
	return true, nil
}

func (store *logStore) read(ctx context.Context, config logger.ReadConfig) *logger.LogWatcher {
	return store.reader.ReadLogs(ctx, config)
}

func (store *logStore) close() error {
	return store.storage.Close()
}

func isPlaceholder(line []byte) bool {
	hasNUL := false
	for _, value := range line {
		switch value {
		case 0:
			hasNUL = true
		case ' ', '\t', '\n', '\r', '\v', '\f':
		default:
			return false
		}
	}
	return hasNUL
}
