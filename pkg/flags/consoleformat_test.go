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
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	logsapi "k8s.io/component-base/logs/api/v1"
)

// entry matches "2026-09-01T16:02:51.179Z\tINFO\t[name\t]consoleformat_test.go:41\t".
var entry = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T[\d:.]+Z\t(INFO|WARN|ERROR|FATAL)\t(\S+\t)?\S+\.go:\d+\t`)

func newConsoleLogger(t *testing.T, verbosity int32) (*bytes.Buffer, logr.Logger) {
	t.Helper()

	buf := &bytes.Buffer{}
	c := logsapi.NewLoggingConfiguration()
	c.Format = ConsoleLogFormat
	c.Verbosity = logsapi.VerbosityLevel(verbosity)

	logger, control := consoleFactory{}.Create(*c, logsapi.LoggingOptions{ErrorStream: buf})
	require.NotNil(t, control.SetVerbosityLevel)
	return buf, logger
}

func lines(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()

	out := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	for _, line := range out {
		require.Regexp(t, entry, line)
	}
	return out
}

// TestConsoleFormat checks the rendering of structured calls and that V()
// levels above the configured verbosity are dropped.
func TestConsoleFormat(t *testing.T) {
	buf, logger := newConsoleLogger(t, 4)

	logger.Info("starting webhook server", "port", 443)
	logger.V(4).Info("handling request")
	logger.V(6).Info("must not appear")
	logger.WithName("webhook").WithValues("claim", "a b").Error(errors.New("boom"), "failed to get clique")

	out := lines(t, buf)
	require.Len(t, out, 3, "V(6) should be dropped at verbosity 4:\n%s", buf)

	require.Contains(t, out[0], "\tINFO\t")
	require.True(t, strings.HasSuffix(out[0], `starting webhook server port="443"`), out[0])
	require.Contains(t, out[2], "\tERROR\twebhook\t")
	require.True(t, strings.HasSuffix(out[2], `failed to get clique claim="a b" err="boom"`), out[2])
}

// TestConsoleFormatKlogBuffer checks that the severity of the printf style klog
// calls survives, in particular Warning and Fatal, which logr cannot express.
func TestConsoleFormatKlogBuffer(t *testing.T) {
	buf, logger := newConsoleLogger(t, 4)
	sink, ok := logger.GetSink().(*consoleSink)
	require.True(t, ok)

	for _, data := range []string{
		"I0901 16:02:51.179000       1 main.go:105] starting webhook server\n",
		"W0901 16:02:51.179000       1 nvlib.go:512] skipping device\n",
		"E0901 16:02:51.179000       1 driver.go:88] prepare failed\n",
		"F0901 16:02:51.179000       1 main.go:60] cannot start\n",
	} {
		sink.WriteKlogBuffer([]byte(data))
	}

	out := lines(t, buf)
	require.Len(t, out, 4)
	require.True(t, strings.HasSuffix(out[0], "INFO\tmain.go:105\tstarting webhook server"), out[0])
	require.True(t, strings.HasSuffix(out[1], "WARN\tnvlib.go:512\tskipping device"), out[1])
	require.True(t, strings.HasSuffix(out[2], "ERROR\tdriver.go:88\tprepare failed"), out[2])
	require.True(t, strings.HasSuffix(out[3], "FATAL\tmain.go:60\tcannot start"), out[3])
}

// TestConsoleFormatVerbosityUpdate checks that SetVerbosityLevel takes effect
// after the logger has been created.
func TestConsoleFormatVerbosityUpdate(t *testing.T) {
	buf := &bytes.Buffer{}
	c := logsapi.NewLoggingConfiguration()
	c.Format = ConsoleLogFormat
	c.Verbosity = 0

	logger, control := consoleFactory{}.Create(*c, logsapi.LoggingOptions{ErrorStream: buf})

	logger.V(3).Info("hidden at verbosity 0")
	require.Empty(t, buf.String())

	require.NoError(t, control.SetVerbosityLevel(3))
	logger.V(3).Info("visible at verbosity 3")
	require.Contains(t, buf.String(), "visible at verbosity 3")
}
