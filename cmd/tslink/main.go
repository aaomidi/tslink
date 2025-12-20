package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/docker/go-plugins-helpers/network"

	"github.com/aaomidi/tslink/pkg/diag"
	"github.com/aaomidi/tslink/pkg/docker"
	"github.com/aaomidi/tslink/pkg/logger"
)

const (
	socketAddress = "/run/docker/plugins/tailscale.sock"
	dataDir       = "/data"
	logPath       = "/data/plugin.log"
)

func main() {
	if err := logger.Init(logPath); err != nil {
		log.Printf("Warning: failed to initialize file logger: %v", err)
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "diag", "diagnose", "status":
			runDiag()
			return
		case "help", "-h", "--help":
			printHelp()
			return
		}
	}

	runPlugin()
}

func runPlugin() {
	logger.Info("Starting Tailscale Docker network plugin")

	driver, err := docker.NewDriver()
	if err != nil {
		logger.Error("Failed to create driver: %v", err)
		os.Exit(1)
	}

	// Set up signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-sigCh
		logger.Info("Received signal %v, initiating graceful shutdown", sig)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := driver.Shutdown(ctx); err != nil {
			logger.Error("Error during shutdown: %v", err)
		}

		logger.Info("Shutdown complete, exiting")
		os.Exit(0)
	}()

	handler := network.NewHandler(driver)

	logger.Info("Listening on %s", socketAddress)
	if err := handler.ServeUnix(socketAddress, 0); err != nil {
		logger.Error("Failed to serve: %v", err)
		os.Exit(1)
	}
}

func runDiag() {
	if err := diag.Run(dataDir, os.Stdout); err != nil {
		log.Fatalf("Diagnostic failed: %v", err)
	}
}

func printHelp() {
	os.Stdout.WriteString(`tslink - Tailscale Container Network Plugin

Usage:
  tslink          Start the plugin server
  tslink diag     Run diagnostics on all endpoints
  tslink help     Show this help message

Diagnostics:
  The 'diag' command shows the health status of all Tailscale endpoints.
  Run it from the host using:

    docker run --rm -v /var/lib/docker-plugins/tailscale:/data \
      --entrypoint /tslink \
      ghcr.io/aaomidi/tslink:latest diag

  Or use the helper script:

    curl -sL https://raw.githubusercontent.com/aaomidi/tslink/main/scripts/tslink-diag.sh | bash

`)
}
