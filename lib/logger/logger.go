package logger

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/buildinfo"
	"github.com/VictoriaMetrics/metrics"
)

var (
	loggerFormat   = flag.String("loggerFormat", "default", "Format for logs. Possible values: default, json")
	loggerOutput   = flag.String("loggerOutput", "stderr", "Output for the logs. Supported values: stderr, stdout")
	loggerTimezone = flag.String("loggerTimezone", "UTC", "Timezone to use for timestamps in logs. Timezone must be a valid IANA Time Zone. "+
		"For example: America/New_York, Europe/Berlin, Etc/GMT+3 or Local")
	disableTimestamps = flag.Bool("loggerDisableTimestamps", false, "Whether to disable writing timestamps in logs")

	errorsPerSecondLimit = flag.Int("loggerErrorsPerSecondLimit", 0, `Per-second limit on the number of ERROR messages. If more than the given number of errors are emitted per second, the remaining errors are suppressed. Zero values disable the rate limit`)
	warnsPerSecondLimit  = flag.Int("loggerWarnsPerSecondLimit", 0, `Per-second limit on the number of WARN messages. If more than the given number of warns are emitted per second, then the remaining warns are suppressed. Zero values disable the rate limit`)
)

// Init initializes the logger.
//
// Init must be called after flag.Parse()
//
// There is no need in calling Init from tests.
func Init() {
	setLoggerJSONFields()
	setLoggerOutput()
	setLoggerLevel()
	validateLoggerFormat()
	initTimezone()
	go logLimiterCleaner()
	logAllFlags()
}

func initTimezone() {
	tz, err := time.LoadLocation(*loggerTimezone)
	if err != nil {
		log.Fatalf("cannot load timezone %q: %s", *loggerTimezone, err)
	}
	timezone = tz
}

var timezone = time.UTC

func setLoggerOutput() {
	switch *loggerOutput {
	case "stderr":
		output = os.Stderr
	case "stdout":
		output = os.Stdout
	default:
		panic(fmt.Errorf("FATAL: unsupported `loggerOutput` value: %q; supported values are: stderr, stdout", *loggerOutput))
	}
}

var output io.Writer = os.Stderr

func validateLoggerFormat() {
	switch *loggerFormat {
	case "default", "json":
	default:
		// We cannot use logger.Panicf here, since the logger isn't initialized yet.
		panic(fmt.Errorf("FATAL: unsupported `-loggerFormat` value: %q; supported values are: default, json", *loggerFormat))
	}
}

var stdErrorLogger = log.New(&stdErrorWriter{}, "", 0)

// StdErrorLogger returns standard error logger.
func StdErrorLogger() *log.Logger {
	return stdErrorLogger
}

// Infof logs info message.
func Infof(format string, args ...interface{}) {
	logf(levelInfo, format, args...)
}

// Warnf logs warn message.
func Warnf(format string, args ...interface{}) {
	logf(levelWarn, format, args...)
}

// Errorf logs error message.
func Errorf(format string, args ...interface{}) {
	logf(levelError, format, args...)
}

// WarnfSkipframes logs warn message and skips the given number of frames for the caller.
func WarnfSkipframes(skipframes int, format string, args ...interface{}) {
	logfSkipframes(skipframes, levelWarn, format, args...)
}

// ErrorfSkipframes logs error message and skips the given number of frames for the caller.
func ErrorfSkipframes(skipframes int, format string, args ...interface{}) {
	logfSkipframes(skipframes, levelError, format, args...)
}

// Fatalf logs fatal message and terminates the app.
func Fatalf(format string, args ...interface{}) {
	logf(levelFatal, format, args...)
}

// Panicf logs panic message and panics.
func Panicf(format string, args ...interface{}) {
	logf(levelPanic, format, args...)
}

func logf(level logLevel, format string, args ...interface{}) {
	logfSkipframes(1, level, format, args...)
}

func logfSkipframes(skipframes int, level logLevel, format string, args ...interface{}) {
	if level < minLogLevel {
		return
	}
	msg := fmt.Sprintf(format, args...)
	logMessage(3+skipframes, level, msg)
}

func logLimiterCleaner() {
	for {
		time.Sleep(time.Second)
		limiter.reset()
	}
}

var limiter = newLogLimiter()

func newLogLimiter() *logLimiter {
	return &logLimiter{
		m: make(map[string]uint64),
	}
}

type logLimiter struct {
	mu sync.Mutex
	m  map[string]uint64
}

func (ll *logLimiter) reset() {
	ll.mu.Lock()
	ll.m = make(map[string]uint64, len(ll.m))
	ll.mu.Unlock()
}

// needSuppress checks if the number of calls for the given location exceeds the given limit.
//
// When the number of calls equals limit, log message prefix returned.
func (ll *logLimiter) needSuppress(limit uint64, location string) (bool, string) {
	// fast path
	var msg string
	if limit == 0 {
		return false, msg
	}
	ll.mu.Lock()
	defer ll.mu.Unlock()

	if n, ok := ll.m[location]; ok {
		if n >= limit {
			switch n {
			// report only once
			case limit:
				msg = fmt.Sprintf("suppressing log message with rate limit=%d: ", limit)
			default:
				return true, msg
			}
		}
		ll.m[location] = n + 1
	} else {
		ll.m[location] = 1
	}
	return false, msg
}

type stdErrorWriter struct {
}

func (lw *stdErrorWriter) Write(p []byte) (int, error) {
	logfSkipframes(2, levelError, "%s", p)
	return len(p), nil
}

func logMessage(skipframes int, level logLevel, msg string) {
	timestamp := ""
	if !*disableTimestamps {
		timestamp = time.Now().In(timezone).Format("2006-01-02T15:04:05.000Z0700")
	}
	_, file, line, ok := runtime.Caller(skipframes)
	if !ok {
		file = "???"
		line = 0
	}
	if n := strings.Index(file, "/VictoriaMetrics/"); n >= 0 {
		// Strip [...]/VictoriaMetrics/ prefix
		file = file[n+len("/VictoriaMetrics/"):]
	}
	location := fmt.Sprintf("%s:%d", file, line)

	// rate limit ERROR and WARN log messages with given limit.
	if level == levelError || level == levelWarn {
		limit := uint64(*errorsPerSecondLimit)
		if level == levelWarn {
			limit = uint64(*warnsPerSecondLimit)
		}
		ok, suppressMessage := limiter.needSuppress(limit, location)
		if ok {
			return
		}
		if len(suppressMessage) > 0 {
			msg = suppressMessage + msg
		}
	}

	for len(msg) > 0 && msg[len(msg)-1] == '\n' {
		msg = msg[:len(msg)-1]
	}
	var logMsg string
	switch *loggerFormat {
	case "json":
		if *disableTimestamps {
			logMsg = fmt.Sprintf(
				`{%q:%q,%q:%q,%q:%q}`+"\n",
				fieldLevel, level,
				fieldCaller, location,
				fieldMsg, msg,
			)
		} else {
			logMsg = fmt.Sprintf(
				`{%q:%q,%q:%q,%q:%q,%q:%q}`+"\n",
				fieldTs, timestamp,
				fieldLevel, level,
				fieldCaller, location,
				fieldMsg, msg,
			)
		}
	default:
		if *disableTimestamps {
			logMsg = fmt.Sprintf("%s\t%s\t%s\n", level, location, msg)
		} else {
			logMsg = fmt.Sprintf("%s\t%s\t%s\t%s\n", timestamp, level, location, msg)
		}
	}

	// Serialize writes to log.
	mu.Lock()
	fmt.Fprint(output, logMsg)
	mu.Unlock()

	// Increment vm_log_messages_total
	counterName := fmt.Sprintf(`vm_log_messages_total{app_version=%q, level=%q, location=%q}`, buildinfo.Version, level, location)
	metrics.GetOrCreateCounter(counterName).Inc()

	switch level {
	case levelPanic:
		if *loggerFormat == "json" {
			// Do not clutter `json` output with panic stack trace
			os.Exit(-1)
		}
		panic(errors.New(msg))
	case levelFatal:
		os.Exit(-1)
	}
}

var mu sync.Mutex

// SetOutputForTests redefine output for logger. Use for Tests only. Call ResetOutputForTest to return output state to default
func SetOutputForTests(writer io.Writer) { output = writer }

// ResetOutputForTest set logger output to default value
func ResetOutputForTest() { output = os.Stderr }
