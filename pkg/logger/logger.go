package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
)

var (
	logger *slog.Logger
	once   sync.Once
)

// Init initializes the global logger with file and stdout output.
// Call this once at startup. If not called, Get() will initialize with defaults.
func Init(logPath string) error {
	var initErr error
	once.Do(func() {
		initErr = initLogger(logPath)
	})
	return initErr
}

func initLogger(logPath string) error {
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		// Fall back to stdout only
		logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		}))
		return err
	}

	// Create multi-writer for both file and stdout
	multiWriter := io.MultiWriter(file, os.Stdout)

	logger = slog.New(slog.NewTextHandler(multiWriter, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	return nil
}

// Get returns the global logger, initializing with defaults if needed.
func Get() *slog.Logger {
	once.Do(func() {
		// Default initialization to /data/plugin.log
		_ = initLogger("/data/plugin.log")
	})
	return logger
}

// Debug logs at debug level with printf-style formatting.
func Debug(format string, args ...any) {
	Get().Debug(fmt.Sprintf(format, args...))
}

// Info logs at info level with printf-style formatting.
func Info(format string, args ...any) {
	Get().Info(fmt.Sprintf(format, args...))
}

// Warn logs at warn level with printf-style formatting.
func Warn(format string, args ...any) {
	Get().Warn(fmt.Sprintf(format, args...))
}

// Error logs at error level with printf-style formatting.
func Error(format string, args ...any) {
	Get().Error(fmt.Sprintf(format, args...))
}

// With returns a logger with additional attributes.
func With(args ...any) *slog.Logger {
	return Get().With(args...)
}
