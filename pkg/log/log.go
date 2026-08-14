// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package log

import (
	"flag"
	"os"
	"strings"

	"github.com/go-logr/zapr"
	zzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	DebugLevel = int(zapcore.DebugLevel)
	// TraceLevel is two verbosity steps below info. logr maps V(1) to zap's
	// debug level (-1) and V(2) to -2, so a dedicated trace level is needed
	// to make V(2) logs selectable from the CLI.
	TraceLevel = zapcore.Level(-2)
)

var (
	logFile        *os.File
	logFileSet     bool
	loggingEnabled bool
)

// Options stores controller-runtime (zap) log config
var Options = &zap.Options{
	Development: true,
	// we dont log with panic level, so this essentially
	// disables stacktrace, for now, it avoids un-needed clutter in logs
	StacktraceLevel: zapcore.DPanicLevel,
	TimeEncoder:     zapcore.RFC3339NanoTimeEncoder,
	Level:           zzap.NewAtomicLevelAt(zapcore.InfoLevel),
	// log caller (file and line number) in "caller" key
	EncoderConfigOptions: []zap.EncoderConfigOption{func(ec *zapcore.EncoderConfig) { ec.CallerKey = "caller" }},
	ZapOpts:              []zzap.Option{zzap.AddCaller()},
}

// BindFlags binds controller-runtime logging flags to provided flag Set
func BindFlags(fs *flag.FlagSet) {
	Options.BindFlags(fs)
}

// SetLogFile configures logging to write to a file
func SetLogFile(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	logFile = file
	logFileSet = true
	return nil
}

// SetLoggingEnabled controls whether logging is enabled or disabled
func SetLoggingEnabled(enabled bool) {
	loggingEnabled = enabled
}

// IsEnabled returns whether logging is currently enabled
func IsEnabled() bool {
	return loggingEnabled
}

// InitLog initializes controller-runtime log (zap log)
// this should be called once Options have been initialized
// either by parsing flags or directly modifying Options.
func InitLog() {
	defer routeNoiseToControllerRuntimeLogger()

	if !loggingEnabled {
		// Disable logging by setting level to panic (effectively disables all logs)
		Options.Level = zzap.NewAtomicLevelAt(zapcore.PanicLevel)
		log.SetLogger(zap.New(zap.UseFlagOptions(Options)))
		return
	}

	if logFileSet {
		// Configure file output
		encoder := zapcore.NewConsoleEncoder(zapcore.EncoderConfig{
			TimeKey:        "time",
			LevelKey:       "level",
			NameKey:        "logger",
			CallerKey:      "caller",
			MessageKey:     "msg",
			StacktraceKey:  "stacktrace",
			LineEnding:     zapcore.DefaultLineEnding,
			EncodeLevel:    encodeLevel,
			EncodeTime:     zapcore.RFC3339NanoTimeEncoder,
			EncodeDuration: zapcore.StringDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
		})

		writeSyncer := zapcore.AddSync(logFile)
		core := zapcore.NewCore(encoder, writeSyncer, Options.Level)
		logger := zzap.New(core, zzap.AddCaller(), zzap.AddStacktrace(zapcore.DPanicLevel))
		log.SetLogger(zapr.NewLogger(logger))
		return
	}

	// Log to stderr (keep stdout clean)
	// Configure stderr output by creating a custom zap core
	encoder := zapcore.NewConsoleEncoder(zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    encodeLevel,
		EncodeTime:     zapcore.RFC3339NanoTimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	})

	writeSyncer := zapcore.AddSync(os.Stderr)
	core := zapcore.NewCore(encoder, writeSyncer, Options.Level)
	logger := zzap.New(core, zzap.AddCaller(), zzap.AddStacktrace(zapcore.DPanicLevel))
	log.SetLogger(zapr.NewLogger(logger))
}

// SetLogLevel sets current logging level to the provided lvl
func SetLogLevel(lvl string) error {
	newLevel, err := parseLevel(lvl)
	if err != nil {
		return err
	}

	currLevel := Options.Level.(zzap.AtomicLevel).Level()

	if newLevel != currLevel {
		log.Log.Info("set log verbose level", "newLevel", levelName(newLevel), "currentLevel", levelName(currLevel))
		Options.Level.(zzap.AtomicLevel).SetLevel(newLevel)
	}
	return nil
}

// GetLogLevel returns the current logging level
func GetLogLevel() string {
	return levelName(Options.Level.(zzap.AtomicLevel).Level())
}

func parseLevel(level string) (zapcore.Level, error) {
	if strings.EqualFold(strings.TrimSpace(level), "trace") {
		return TraceLevel, nil
	}
	return zapcore.ParseLevel(level)
}

func levelName(level zapcore.Level) string {
	if level == TraceLevel {
		return "trace"
	}
	return level.String()
}

func encodeLevel(level zapcore.Level, encoder zapcore.PrimitiveArrayEncoder) {
	if level == TraceLevel {
		encoder.AppendString("TRACE")
		return
	}
	zapcore.CapitalLevelEncoder(level, encoder)
}

// routeNoiseToControllerRuntimeLogger silences two upstream sources of
// chatter that would otherwise leak to stderr at info level:
//
//   - k8s.io/client-go's HTTP-299 warning handler (kube API
//     deprecations like "node-role.kubernetes.io/master is deprecated").
//     The default handler prints to klog; we route it through our
//     logr.Logger at V(2) so the warnings stay visible under
//     --log-level=trace but disappear otherwise.
//   - klog itself, which client-go and the helm SDK both initialise.
//     Without this, klog still prints `I0524 18:40:18.932 warnings.go:110]`
//     to stderr even when our zap logger is configured to be quiet.
//
// Must be called AFTER log.SetLogger so the controller-runtime logger
// is already installed.
func routeNoiseToControllerRuntimeLogger() {
	rest.SetDefaultWarningHandler(&restWarningRouter{})
	klog.SetLogger(log.Log.V(2))
}

// restWarningRouter forwards client-go HTTP-299 warning headers to the
// controller-runtime logger at V(2). At info-level the calls are no-ops
// (zap filters them); at trace-level they surface alongside the rest of
// our structured logs.
type restWarningRouter struct{}

func (restWarningRouter) HandleWarningHeader(code int, agent string, text string) {
	if code != 299 || text == "" {
		return
	}
	log.Log.V(2).Info("kube API warning", "agent", agent, "text", text)
}
