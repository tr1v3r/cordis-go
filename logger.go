package cordis

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Level is a log severity.
type Level int

// Log levels.
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelSilent
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "silent"
	}
}

type loggerService struct {
	mu    sync.Mutex
	wmu   sync.Mutex
	w     io.Writer
	level Level
}

func newLoggerService(w io.Writer, level Level) *loggerService {
	if w == nil {
		w = io.Discard
	}
	return &loggerService{w: w, level: level}
}

func (s *loggerService) logf(level Level, name, format string, args ...any) {
	s.mu.Lock()
	if level < s.level {
		s.mu.Unlock()
		return
	}
	writer := s.w
	s.mu.Unlock()

	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	line := fmt.Sprintf("%s [%s] %s: %s\n", time.Now().Format("15:04:05.000"), level, name, message)

	// Writes are serialized separately so a writer that logs re-entrantly
	// cannot deadlock against the level/config lock.
	s.wmu.Lock()
	defer s.wmu.Unlock()
	fmt.Fprint(writer, line)
}

func (s *loggerService) errorf(format string, args ...any) {
	s.logf(LevelError, "cordis", format, args...)
}

// Logger is a named handle onto the application logger.
type Logger struct {
	name string
	svc  *loggerService
}

// Name returns the logger name.
func (l *Logger) Name() string { return l.name }

// Debug logs at debug level.
func (l *Logger) Debug(format string, args ...any) { l.svc.logf(LevelDebug, l.name, format, args...) }

// Info logs at info level.
func (l *Logger) Info(format string, args ...any) { l.svc.logf(LevelInfo, l.name, format, args...) }

// Warn logs at warn level.
func (l *Logger) Warn(format string, args ...any) { l.svc.logf(LevelWarn, l.name, format, args...) }

// Error logs at error level.
func (l *Logger) Error(format string, args ...any) { l.svc.logf(LevelError, l.name, format, args...) }
