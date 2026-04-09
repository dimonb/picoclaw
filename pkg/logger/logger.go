package logger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/trace"
)

type LogLevel = zerolog.Level

const (
	DEBUG = zerolog.DebugLevel
	INFO  = zerolog.InfoLevel
	WARN  = zerolog.WarnLevel
	ERROR = zerolog.ErrorLevel
	FATAL = zerolog.FatalLevel
)

var (
	logger       zerolog.Logger
	fileLogger   zerolog.Logger
	currentLevel LogLevel = zerolog.InfoLevel
	mu           sync.Mutex
)

func init() {
	// Default console logger
	logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).With().Timestamp().Logger()
	fileLogger = zerolog.New(nil).Level(zerolog.NoLevel)
}

func SetLevel(level LogLevel) {
	mu.Lock()
	defer mu.Unlock()
	currentLevel = level
	logger = logger.Level(level)
}

func SetLevelFromString(level string) {
	switch strings.ToUpper(level) {
	case "DEBUG":
		SetLevel(DEBUG)
	case "INFO":
		SetLevel(INFO)
	case "WARN":
		SetLevel(WARN)
	case "ERROR":
		SetLevel(ERROR)
	case "FATAL":
		SetLevel(FATAL)
	}
}

func EnableFileLogging(path string) error {
	mu.Lock()
	defer mu.Unlock()

	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	fileLogger = zerolog.New(f).With().Timestamp().Logger().Level(currentLevel)
	return nil
}

func DisableFileLogging() {
	mu.Lock()
	defer mu.Unlock()
	fileLogger = zerolog.New(nil).Level(zerolog.NoLevel)
}

func ConfigureFromEnv() {
	mu.Lock()
	defer mu.Unlock()

	level := os.Getenv("LOG_LEVEL")
	if level != "" {
		SetLevelFromString(level)
	}

	logPath := os.Getenv("LOG_FILE")
	if logPath != "" {
		_ = EnableFileLogging(logPath)
	}
}

// Public functions

func Debug(message string) { logMessage(nil, DEBUG, "", message, nil) }
func Info(message string)  { logMessage(nil, INFO, "", message, nil) }
func Warn(message string)  { logMessage(nil, WARN, "", message, nil) }
func Error(message string) { logMessage(nil, ERROR, "", message, nil) }
func Fatal(message string) { logMessage(nil, FATAL, "", message, nil) }

func DebugF(message string, fields map[string]any) { logMessage(nil, DEBUG, "", message, fields) }
func InfoF(message string, fields map[string]any)  { logMessage(nil, INFO, "", message, fields) }
func WarnF(message string, fields map[string]any)  { logMessage(nil, WARN, "", message, fields) }
func ErrorF(message string, fields map[string]any) { logMessage(nil, ERROR, "", message, fields) }
func FatalF(message string, fields map[string]any) { logMessage(nil, FATAL, "", message, fields) }

func DebugC(component string, message string) { logMessage(nil, DEBUG, component, message, nil) }
func InfoC(component string, message string)  { logMessage(nil, INFO, component, message, nil) }
func WarnC(component string, message string)  { logMessage(nil, WARN, component, message, nil) }
func ErrorC(component string, message string) { logMessage(nil, ERROR, component, message, nil) }
func FatalC(component string, message string) { logMessage(nil, FATAL, component, message, nil) }

func DebugCF(component string, message string, fields map[string]any) {
	logMessage(nil, DEBUG, component, message, fields)
}
func InfoCF(component string, message string, fields map[string]any) {
	logMessage(nil, INFO, component, message, fields)
}
func WarnCF(component string, message string, fields map[string]any) {
	logMessage(nil, WARN, component, message, fields)
}
func ErrorCF(component string, message string, fields map[string]any) {
	logMessage(nil, ERROR, component, message, fields)
}
func FatalCF(component string, message string, fields map[string]any) {
	logMessage(nil, FATAL, component, message, fields)
}

func Debugf(format string, v ...any) { logMessage(nil, DEBUG, "", fmt.Sprintf(format, v...), nil) }
func Infof(format string, v ...any)  { logMessage(nil, INFO, "", fmt.Sprintf(format, v...), nil) }
func Warnf(format string, v ...any)  { logMessage(nil, WARN, "", fmt.Sprintf(format, v...), nil) }
func Errorf(format string, v ...any) { logMessage(nil, ERROR, "", fmt.Sprintf(format, v...), nil) }
func Fatalf(format string, v ...any) { logMessage(nil, FATAL, "", fmt.Sprintf(format, v...), nil) }

// Context-aware public functions

func DebugCtx(ctx context.Context, message string) { logMessage(ctx, DEBUG, "", message, nil) }
func InfoCtx(ctx context.Context, message string)  { logMessage(ctx, INFO, "", message, nil) }
func WarnCtx(ctx context.Context, message string)  { logMessage(ctx, WARN, "", message, nil) }
func ErrorCtx(ctx context.Context, message string) { logMessage(ctx, ERROR, "", message, nil) }
func FatalCtx(ctx context.Context, message string) { logMessage(ctx, FATAL, "", message, nil) }

func DebugFCtx(ctx context.Context, message string, fields map[string]any) {
	logMessage(ctx, DEBUG, "", message, fields)
}
func InfoFCtx(ctx context.Context, message string, fields map[string]any) {
	logMessage(ctx, INFO, "", message, fields)
}
func WarnFCtx(ctx context.Context, message string, fields map[string]any) {
	logMessage(ctx, WARN, "", message, fields)
}
func ErrorFCtx(ctx context.Context, message string, fields map[string]any) {
	logMessage(ctx, ERROR, "", message, fields)
}
func FatalFCtx(ctx context.Context, message string, fields map[string]any) {
	logMessage(ctx, FATAL, "", message, fields)
}

func DebugCCtx(ctx context.Context, component string, message string) {
	logMessage(ctx, DEBUG, component, message, nil)
}
func InfoCCtx(ctx context.Context, component string, message string) {
	logMessage(ctx, INFO, component, message, nil)
}
func WarnCCtx(ctx context.Context, component string, message string) {
	logMessage(ctx, WARN, component, message, nil)
}
func ErrorCCtx(ctx context.Context, component string, message string) {
	logMessage(ctx, ERROR, component, message, nil)
}
func FatalCCtx(ctx context.Context, component string, message string) {
	logMessage(ctx, FATAL, component, message, nil)
}

func DebugCFCtx(ctx context.Context, component string, message string, fields map[string]any) {
	logMessage(ctx, DEBUG, component, message, fields)
}
func InfoCFCtx(ctx context.Context, component string, message string, fields map[string]any) {
	logMessage(ctx, INFO, component, message, fields)
}
func WarnCFCtx(ctx context.Context, component string, message string, fields map[string]any) {
	logMessage(ctx, WARN, component, message, fields)
}
func ErrorCFCtx(ctx context.Context, component string, message string, fields map[string]any) {
	logMessage(ctx, ERROR, component, message, fields)
}
func FatalCFCtx(ctx context.Context, component string, message string, fields map[string]any) {
	logMessage(ctx, FATAL, component, message, fields)
}

// Internal implementation

func logMessage(ctx context.Context, level LogLevel, component string, message string, fields map[string]any) {
	if level < currentLevel {
		return
	}

	// 1. Emit to OpenTelemetry Logs
	otelLog(ctx, level, component, message, fields)

	// 2. Add trace correlation for local logs
	if ctx != nil {
		spanContext := trace.SpanFromContext(ctx).SpanContext()
		if spanContext.IsValid() {
			if fields == nil {
				fields = make(map[string]any)
			}
			fields["trace_id"] = spanContext.TraceID().String()
			fields["span_id"] = spanContext.SpanID().String()
		}
	}

	// 3. Emit to Zerolog
	skip := getCallerSkip()
	event := getEvent(logger, level)
	if component != "" {
		event.Str("component", component)
	}
	appendFields(event, fields)
	event.CallerSkipFrame(skip).Msg(message)

	// Also log to file if enabled
	if fileLogger.GetLevel() != zerolog.NoLevel {
		fileEvent := getEvent(fileLogger, level)
		if component != "" {
			fileEvent.Str("component", component)
		}
		appendFields(fileEvent, fields)
		fileEvent.CallerSkipFrame(skip).Msg(message)
	}

	if level == FATAL {
		os.Exit(1)
	}
}

func otelLog(ctx context.Context, level LogLevel, component string, message string, fields map[string]any) {
	if ctx == nil {
		ctx = context.Background()
	}
	l := global.GetLoggerProvider().Logger("picoclaw")
	var record log.Record
	record.SetBody(log.StringValue(message))

	severity := log.SeverityInfo
	switch level {
	case zerolog.DebugLevel:
		severity = log.SeverityDebug
	case zerolog.WarnLevel:
		severity = log.SeverityWarn
	case zerolog.ErrorLevel:
		severity = log.SeverityError
	case zerolog.FatalLevel:
		severity = log.SeverityFatal
	case zerolog.PanicLevel:
		severity = log.SeverityFatal
	}
	record.SetSeverity(severity)
	record.SetSeverityText(level.String())

	if component != "" {
		record.AddAttributes(log.String("component", component))
	}

	for k, v := range fields {
		switch val := v.(type) {
		case string:
			record.AddAttributes(log.String(k, val))
		case int:
			record.AddAttributes(log.Int(k, val))
		case bool:
			record.AddAttributes(log.Bool(k, val))
		case float64:
			record.AddAttributes(log.Float64(k, val))
		default:
			record.AddAttributes(log.String(k, fmt.Sprintf("%v", v)))
		}
	}

	l.Emit(ctx, record)
}

func getCallerSkip() int {
	for i := 2; i < 15; i++ {
		pc, file, _, ok := runtime.Caller(i)
		if !ok {
			continue
		}

		fn := runtime.FuncForPC(pc)
		if fn == nil {
			continue
		}

		// bypass common loggers
		if strings.HasSuffix(file, "/logger.go") ||
			strings.HasSuffix(file, "/zerolog/log.go") {
			continue
		}

		funcName := fn.Name()
		if strings.HasPrefix(funcName, "runtime.") {
			continue
		}

		return i - 1
	}

	return 3
}

//nolint:zerologlint
func getEvent(logger zerolog.Logger, level LogLevel) *zerolog.Event {
	switch level {
	case zerolog.DebugLevel:
		return logger.Debug()
	case zerolog.InfoLevel:
		return logger.Info()
	case zerolog.WarnLevel:
		return logger.Warn()
	case zerolog.ErrorLevel:
		return logger.Error()
	case zerolog.FatalLevel:
		return logger.Fatal()
	default:
		return logger.Info()
	}
}

func appendFields(event *zerolog.Event, fields map[string]any) {
	for k, v := range fields {
		switch val := v.(type) {
		case error:
			event.Err(val)
		case string:
			event.Str(k, val)
		case int:
			event.Int(k, val)
		case int64:
			event.Int64(k, val)
		case bool:
			event.Bool(k, val)
		default:
			event.Interface(k, v)
		}
	}
}
