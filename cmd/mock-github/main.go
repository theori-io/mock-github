package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	mockgithub "github.com/theori-io/mock-github"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "mock-github:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("mock-github", flag.ContinueOnError)
	flags.SetOutput(stderr)
	addr := flags.String("addr", "127.0.0.1:8080", "listen address; use 127.0.0.1:0 for an ephemeral port")
	fixture := flags.String("fixture", "", "JSON fixture file")
	readyFile := flags.String("ready-file", "", "write startup JSON to this file after binding the port")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	cfg := mockgithub.Config{}
	if *fixture != "" {
		file, err := os.Open(*fixture)
		if err != nil {
			return err
		}
		cfg, err = mockgithub.DecodeConfig(file)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if token := os.Getenv("GITHUB_MOCK_TOKEN"); token != "" {
		cfg.Token = token
	}
	mock, err := mockgithub.New(cfg)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return err
	}
	if host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ready, err := json.Marshal(struct {
		URL string `json:"url"`
	}{URL: "http://" + net.JoinHostPort(host, port)})
	if err != nil {
		return err
	}
	ready = append(ready, '\n')
	if *readyFile != "" {
		// Atomic rename prevents test runners from observing partial JSON.
		temporary := *readyFile + ".tmp"
		if err := os.WriteFile(temporary, ready, 0600); err != nil {
			return err
		}
		if err := os.Rename(temporary, *readyFile); err != nil {
			return err
		}
		defer os.Remove(*readyFile)
	}
	if _, err := stdout.Write(ready); err != nil {
		return err
	}
	server := &http.Server{Handler: mock, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: time.Minute,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}
