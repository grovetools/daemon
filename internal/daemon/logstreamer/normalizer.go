// Package logstreamer provides log file tailing, normalization, and SSE broadcasting for daemon jobs.
package logstreamer

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/grovetools/core/pkg/models"
)

// NormalizerWrapper wraps log line normalization logic.
// For agent transcript files (JSONL), it extracts timestamps and passes through raw JSON.
// For plain text log files, it wraps lines with a timestamp.
//
// Note: The full agentlogs normalizer pipeline (ClaudeNormalizer, CodexNormalizer)
// lives in agentlogs/internal/transcript and cannot be imported cross-module.
// Clients that need rich normalization should apply it on their end using
// the agentlogs library directly. The daemon streams raw lines for maximum
// flexibility and minimal coupling.
type NormalizerWrapper struct {
	provider string
	isAgent  bool // true for JSONL agent transcripts, false for plain text
}

// NewNormalizerForPath creates a NormalizerWrapper appropriate for the given log file path.
func NewNormalizerForPath(logFilePath string) *NormalizerWrapper {
	provider := "text"
	isAgent := false

	if strings.HasSuffix(logFilePath, ".jsonl") {
		isAgent = true
		if strings.Contains(logFilePath, "/.codex/") {
			provider = "codex"
		} else if strings.Contains(logFilePath, "/opencode/") {
			provider = "opencode"
		} else {
			provider = "claude"
		}
	}

	return &NormalizerWrapper{
		provider: provider,
		isAgent:  isAgent,
	}
}

// NormalizeLine takes a raw line from a log file and returns a structured LogLine.
// For JSONL agent transcripts, it extracts the timestamp from the JSON if present.
// For plain text logs, it wraps the line with the current timestamp.
// Returns nil for empty lines.
func (nw *NormalizerWrapper) NormalizeLine(rawLine []byte) *models.LogLine {
	line := strings.TrimSpace(string(rawLine))
	if len(line) == 0 {
		return nil
	}

	if nw.isAgent {
		// For JSONL agent transcripts, attempt to extract a timestamp
		ts := time.Now()
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &raw) == nil {
			if tsRaw, ok := raw["timestamp"]; ok {
				var tsStr string
				if json.Unmarshal(tsRaw, &tsStr) == nil {
					if parsed, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
						ts = parsed
					}
				}
			}
		}
		return &models.LogLine{
			Line:      line,
			Timestamp: ts,
		}
	}

	// Plain text log — wrap with current timestamp
	return &models.LogLine{
		Line:      line,
		Timestamp: time.Now(),
	}
}

// Provider returns the detected provider name.
func (nw *NormalizerWrapper) Provider() string {
	return nw.provider
}
