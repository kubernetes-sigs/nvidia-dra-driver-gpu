/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package flags

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2/textlogger"
)

// ConsoleLogFormat is the name of the human readable, tab separated log format
// selectable via --logging-format=console:
//
//	2026-09-01T16:02:51.179Z	INFO	main.go:105	starting webhook server
const ConsoleLogFormat = "console"

const consoleTimeFormat = "2006-01-02T15:04:05.000Z"

func init() {
	// Console is an optional klog format, gated the same way the upstream JSON
	// format is, just at alpha rather than beta.
	if err := logsapi.RegisterLogFormat(ConsoleLogFormat, consoleFactory{}, logsapi.LoggingAlphaOptions); err != nil {
		panic(err)
	}
}

type consoleFactory struct{}

var _ logsapi.LogFormatFactory = consoleFactory{}

func (consoleFactory) Create(c logsapi.LoggingConfiguration, o logsapi.LoggingOptions) (logr.Logger, logsapi.RuntimeControl) {
	sink := &consoleSink{shared: &consoleShared{out: o.ErrorStream}}
	sink.shared.verbosity.Store(int32(c.Verbosity))

	return logr.New(sink), logsapi.RuntimeControl{
		SetVerbosityLevel: func(v uint32) error {
			sink.shared.verbosity.Store(int32(v))
			return nil
		},
	}
}

// consoleShared holds the state that all loggers derived from the initial one
// have in common.
type consoleShared struct {
	mutex     sync.Mutex
	out       io.Writer
	verbosity atomic.Int32
}

type consoleSink struct {
	shared    *consoleShared
	name      string
	values    []any
	callDepth int
}

var (
	_ logr.LogSink                = &consoleSink{}
	_ logr.CallDepthLogSink       = &consoleSink{}
	_ textlogger.KlogBufferWriter = &consoleSink{}
)

func (s *consoleSink) Init(info logr.RuntimeInfo) {
	s.callDepth += info.CallDepth
}

func (s *consoleSink) Enabled(level int) bool {
	return int32(level) <= s.shared.verbosity.Load()
}

func (s *consoleSink) Info(_ int, msg string, kv ...any) {
	s.print("INFO", s.caller(), msg, kv)
}

func (s *consoleSink) Error(err error, msg string, kv ...any) {
	if err != nil {
		kv = append([]any{"err", err}, kv...)
	}
	s.print("ERROR", s.caller(), msg, kv)
}

func (s *consoleSink) WithValues(kv ...any) logr.LogSink {
	clone := *s
	clone.values = append(append([]any{}, s.values...), kv...)
	return &clone
}

func (s *consoleSink) WithName(name string) logr.LogSink {
	clone := *s
	if s.name != "" {
		name = s.name + "." + name
	}
	clone.name = name
	return &clone
}

func (s *consoleSink) WithCallDepth(depth int) logr.LogSink {
	clone := *s
	clone.callDepth += depth
	return &clone
}

// WriteKlogBuffer renders a buffer that klog has already formatted. Taking this
// path keeps the severity of klog.Warningf and klog.Fatalf, which the logr API
// has no level for and would otherwise report as INFO.
func (s *consoleSink) WriteKlogBuffer(data []byte) {
	caller, msg := splitKlogHeader(data)
	s.print(klogSeverity(data), caller, msg, nil)
}

// splitKlogHeader splits a klog buffer, "Lmmdd hh:mm:ss.uuuuuu goid file:line]
// msg", into its caller and message.
func splitKlogHeader(data []byte) (caller, msg string) {
	end := bytes.IndexByte(data, ']')
	if end < 0 {
		return "", strings.TrimRight(string(data), "\n")
	}
	if start := bytes.LastIndexByte(data[:end], ' '); start >= 0 {
		caller = string(data[start+1 : end])
	}
	return caller, strings.TrimRight(strings.TrimPrefix(string(data[end+1:]), " "), "\n")
}

func klogSeverity(data []byte) string {
	if len(data) == 0 {
		return "INFO"
	}
	switch data[0] {
	case 'W':
		return "WARN"
	case 'E':
		return "ERROR"
	case 'F':
		return "FATAL"
	default:
		return "INFO"
	}
}

func (s *consoleSink) print(level, caller, msg string, kv []any) {
	var buf bytes.Buffer
	buf.WriteString(time.Now().UTC().Format(consoleTimeFormat))
	buf.WriteByte('\t')
	buf.WriteString(level)
	buf.WriteByte('\t')
	if s.name != "" {
		buf.WriteString(s.name)
		buf.WriteByte('\t')
	}
	buf.WriteString(caller)
	buf.WriteByte('\t')
	buf.WriteString(msg)
	writePairs(&buf, s.values)
	writePairs(&buf, kv)
	buf.WriteByte('\n')

	s.shared.mutex.Lock()
	defer s.shared.mutex.Unlock()
	_, _ = s.shared.out.Write(buf.Bytes())
}

// writePairs appends key/value pairs, quoted so that a value can never break
// the one entry per line layout.
func writePairs(buf *bytes.Buffer, kv []any) {
	for i := 0; i < len(kv); i += 2 {
		value := "(MISSING)"
		if i+1 < len(kv) {
			value = fmt.Sprint(kv[i+1])
		}
		_, _ = fmt.Fprintf(buf, " %v=%q", kv[i], value)
	}
}

func (s *consoleSink) caller() string {
	_, file, line, ok := runtime.Caller(s.callDepth + 2)
	if !ok {
		return "unknown"
	}
	if i := strings.LastIndexByte(file, '/'); i >= 0 {
		file = file[i+1:]
	}
	return fmt.Sprintf("%s:%d", file, line)
}
