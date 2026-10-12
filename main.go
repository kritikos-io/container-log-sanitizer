package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/plugins/logdriver"
	"github.com/docker/docker/daemon/logger"
)

const defaultSocketPath = "/run/docker/plugins/sanitizer.sock"

type pluginResponse struct {
	Err string
}

func respond(writer http.ResponseWriter, err error) {
	writer.Header().Set("Content-Type", "application/json")
	response := pluginResponse{}
	if err != nil {
		response.Err = err.Error()
	}
	json.NewEncoder(writer).Encode(response)
}

func decodeRequest(writer http.ResponseWriter, request *http.Request, target any) error {
	if request.Method != http.MethodPost {
		return errors.New("plugin RPC requires POST")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid plugin request: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected exactly one JSON request")
	}
	return nil
}

func newHandler(driver *driver) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/Plugin.Activate", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/vnd.docker.plugins.v1.1+json")
		json.NewEncoder(writer).Encode(map[string]any{"Implements": []string{"LogDriver"}})
	})
	mux.HandleFunc("/LogDriver.Capabilities", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(struct{ Cap logger.Capability }{Cap: logger.Capability{ReadLogs: true}})
	})
	mux.HandleFunc("/LogDriver.StartLogging", func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			File string
			Info logger.Info
		}
		if err := decodeRequest(writer, request, &input); err != nil {
			respond(writer, err)
			return
		}
		respond(writer, driver.StartLogging(input.File, input.Info))
	})
	mux.HandleFunc("/LogDriver.StopLogging", func(writer http.ResponseWriter, request *http.Request) {
		var input struct{ File string }
		if err := decodeRequest(writer, request, &input); err != nil {
			respond(writer, err)
			return
		}
		respond(writer, driver.StopLogging(input.File))
	})
	mux.HandleFunc("/LogDriver.ReadLogs", func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			Info   logger.Info
			Config logger.ReadConfig
		}
		if err := decodeRequest(writer, request, &input); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		if input.Config.Tail < -1 || (!input.Config.Until.IsZero() && input.Config.Until.Before(input.Config.Since)) {
			http.Error(writer, "invalid read interval or tail", http.StatusBadRequest)
			return
		}
		store, active, err := driver.openReader(input.Info.ContainerID)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusNotFound)
			return
		}
		if !active {
			defer store.close()
			input.Config.Follow = false
		}
		watcher := store.read(request.Context(), input.Config)
		defer watcher.ConsumerGone()
		writer.Header().Set("Content-Type", "application/x-json-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		encoder := logdriver.NewLogEntryEncoder(writer)
		for {
			select {
			case <-request.Context().Done():
				return
			case err := <-watcher.Err:
				driver.logger.Printf("container %s: log read failed: %v", input.Info.ContainerID, err)
				return
			case message, open := <-watcher.Msg:
				if !open {
					return
				}
				entry := &logdriver.LogEntry{Line: message.Line, Source: message.Source, TimeNano: message.Timestamp.UnixNano(), Partial: message.PLogMetaData != nil}
				if metadata := message.PLogMetaData; metadata != nil {
					entry.PartialLogMetadata = &logdriver.PartialLogEntryMetadata{Id: metadata.ID, Ordinal: int32(metadata.Ordinal), Last: metadata.Last}
				}
				if err := encoder.Encode(entry); err != nil {
					return
				}
				if flusher, ok := writer.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	})
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]uint64{"stored": driver.stored.Load(), "dropped_placeholders": driver.dropped.Load(), "failed_streams": driver.failed.Load()})
	})
	return mux
}

func run() error {
	socket := flag.String("socket", defaultSocketPath, "plugin Unix socket")
	root := flag.String("root", "/var/lib/container-log-sanitizer", "persistent log directory")
	flag.Parse()
	driver, err := newDriver(*root, log.Default())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*socket), 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(*socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("refusing to replace a non-socket path")
		}
		connection, err := net.DialTimeout("unix", *socket, time.Second)
		if err == nil {
			connection.Close()
			return errors.New("plugin socket is already in use")
		}
		if err := os.Remove(*socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(*socket, 0600); err != nil {
		return err
	}
	server := &http.Server{Handler: newHandler(driver), ReadHeaderTimeout: 5 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	log.Printf("sanitizer listening on %s; log root %s", *socket, *root)
	select {
	case err := <-finished:
		driver.Close()
		return err
	case <-ctx.Done():
		driver.Close()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		return server.Shutdown(shutdown)
	}
}

func main() {
	if err := run(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
