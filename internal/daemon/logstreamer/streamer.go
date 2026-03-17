package logstreamer

import (
	"bufio"
	"context"
	"io"
	"os"
	"sync"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/sirupsen/logrus"
)

// LogStreamer manages log file tailing, caching via ring buffers, and broadcasting
// log events to SSE subscribers on a per-job basis.
type LogStreamer struct {
	store             *store.Store
	streams           map[string]*JobStream
	mu                sync.Mutex
	bufferSize        int
	maxSubscribers    int
	tailPollInterval  time.Duration
	logger            *logrus.Entry
}

// JobStream holds the state for tailing a single job's log file.
type JobStream struct {
	jobID       string
	logFilePath string
	buffer      []models.LogLine // Ring buffer
	bufferIdx   int              // Next write position in ring buffer
	bufferFull  bool             // Whether buffer has wrapped
	subscribers map[chan models.JobStreamEvent]struct{}
	cancel      context.CancelFunc
	mu          sync.Mutex
}

// New creates a new LogStreamer.
func New(st *store.Store, bufferSize, maxSubscribers int, tailPollInterval time.Duration) *LogStreamer {
	if bufferSize <= 0 {
		bufferSize = 1000
	}
	if maxSubscribers <= 0 {
		maxSubscribers = 10
	}
	if tailPollInterval <= 0 {
		tailPollInterval = 500 * time.Millisecond
	}
	return &LogStreamer{
		store:            st,
		streams:          make(map[string]*JobStream),
		bufferSize:       bufferSize,
		maxSubscribers:   maxSubscribers,
		tailPollInterval: tailPollInterval,
		logger:           logging.NewLogger("logstreamer"),
	}
}

// Subscribe creates a new subscription to a job's log stream.
// Returns the historical log buffer and a channel for new events.
// The caller must call Unsubscribe when done.
func (ls *LogStreamer) Subscribe(jobID string, logFilePath string) ([]models.LogLine, chan models.JobStreamEvent, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	stream, exists := ls.streams[jobID]
	if !exists {
		stream = ls.createStream(jobID, logFilePath)
		ls.streams[jobID] = stream
	}

	stream.mu.Lock()
	defer stream.mu.Unlock()

	if len(stream.subscribers) >= ls.maxSubscribers {
		return nil, nil, ErrMaxSubscribers
	}

	ch := make(chan models.JobStreamEvent, 100)
	stream.subscribers[ch] = struct{}{}

	// Return historical buffer
	history := stream.getBufferedLines()

	return history, ch, nil
}

// Unsubscribe removes a subscriber from a job's log stream.
func (ls *LogStreamer) Unsubscribe(jobID string, ch chan models.JobStreamEvent) {
	ls.mu.Lock()
	stream, exists := ls.streams[jobID]
	ls.mu.Unlock()

	if !exists {
		return
	}

	stream.mu.Lock()
	delete(stream.subscribers, ch)
	remaining := len(stream.subscribers)
	stream.mu.Unlock()

	// If no subscribers left and job is terminal, clean up
	if remaining == 0 {
		job := ls.store.GetJob(jobID)
		if job != nil && isTerminalStatus(job.Status) {
			ls.mu.Lock()
			if stream.cancel != nil {
				stream.cancel()
			}
			delete(ls.streams, jobID)
			ls.mu.Unlock()
		}
	}
}

// GetLogs returns the buffered log lines for a job.
func (ls *LogStreamer) GetLogs(jobID string) []models.LogLine {
	ls.mu.Lock()
	stream, exists := ls.streams[jobID]
	ls.mu.Unlock()

	if !exists {
		return nil
	}

	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.getBufferedLines()
}

// Stop cancels all active tailing goroutines.
func (ls *LogStreamer) Stop() {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	for _, stream := range ls.streams {
		if stream.cancel != nil {
			stream.cancel()
		}
		stream.mu.Lock()
		for ch := range stream.subscribers {
			close(ch)
		}
		stream.subscribers = make(map[chan models.JobStreamEvent]struct{})
		stream.mu.Unlock()
	}
	ls.streams = make(map[string]*JobStream)
}

// createStream initializes a new JobStream and starts the tailing goroutine.
// Must be called with ls.mu held.
func (ls *LogStreamer) createStream(jobID string, logFilePath string) *JobStream {
	ctx, cancel := context.WithCancel(context.Background())

	stream := &JobStream{
		jobID:       jobID,
		logFilePath: logFilePath,
		buffer:      make([]models.LogLine, ls.bufferSize),
		subscribers: make(map[chan models.JobStreamEvent]struct{}),
		cancel:      cancel,
	}

	go ls.tailJob(ctx, stream)
	return stream
}

// tailJob is the main tailing goroutine for a single job's log file.
// It reads new lines, normalizes them, appends to the ring buffer, and broadcasts to subscribers.
func (ls *LogStreamer) tailJob(ctx context.Context, stream *JobStream) {
	logger := ls.logger.WithField("job_id", stream.jobID)

	// Wait for the log file to exist
	var file *os.File
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var err error
		file, err = os.Open(stream.logFilePath)
		if err == nil {
			break
		}
		// Check if job is already terminal (no log file will appear)
		job := ls.store.GetJob(stream.jobID)
		if job != nil && isTerminalStatus(job.Status) {
			ls.broadcastStatus(stream, job.Status, job.Error)
			return
		}
		time.Sleep(ls.tailPollInterval)
	}
	defer file.Close()

	logger.WithField("path", stream.logFilePath).Debug("Tailing log file")

	// Create normalizer based on file path
	normalizer := NewNormalizerForPath(stream.logFilePath)
	reader := bufio.NewReader(file)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			// Check if job has completed
			job := ls.store.GetJob(stream.jobID)
			if job != nil && isTerminalStatus(job.Status) {
				// Read any remaining content before exiting
				for {
					remaining, readErr := reader.ReadBytes('\n')
					if len(remaining) > 0 {
						ls.processLine(stream, normalizer, remaining)
					}
					if readErr != nil {
						break
					}
				}
				ls.broadcastStatus(stream, job.Status, job.Error)
				ls.closeSubscribers(stream)
				return
			}
			time.Sleep(ls.tailPollInterval)
			continue
		}
		if err != nil {
			logger.WithError(err).Error("Error reading log file")
			return
		}

		ls.processLine(stream, normalizer, line)
	}
}

// processLine normalizes a raw log line and broadcasts it.
func (ls *LogStreamer) processLine(stream *JobStream, normalizer *NormalizerWrapper, line []byte) {
	logLine := normalizer.NormalizeLine(line)
	if logLine == nil {
		return
	}

	stream.mu.Lock()
	// Append to ring buffer
	stream.buffer[stream.bufferIdx] = *logLine
	stream.bufferIdx++
	if stream.bufferIdx >= len(stream.buffer) {
		stream.bufferIdx = 0
		stream.bufferFull = true
	}

	// Broadcast to all subscribers (non-blocking)
	event := models.JobStreamEvent{
		Event: "log",
		Line:  logLine,
	}
	for ch := range stream.subscribers {
		select {
		case ch <- event:
		default:
			// Drop if subscriber is slow — prevents blocking
		}
	}
	stream.mu.Unlock()
}

// broadcastStatus sends a status event to all subscribers.
func (ls *LogStreamer) broadcastStatus(stream *JobStream, status, errMsg string) {
	stream.mu.Lock()
	defer stream.mu.Unlock()

	event := models.JobStreamEvent{
		Event:  "status",
		Status: status,
		Error:  errMsg,
	}
	for ch := range stream.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}

// closeSubscribers closes all subscriber channels for a stream.
func (ls *LogStreamer) closeSubscribers(stream *JobStream) {
	stream.mu.Lock()
	defer stream.mu.Unlock()

	for ch := range stream.subscribers {
		close(ch)
	}
	stream.subscribers = make(map[chan models.JobStreamEvent]struct{})
}

// getBufferedLines returns the log lines currently in the ring buffer, in order.
// Must be called with stream.mu held.
func (js *JobStream) getBufferedLines() []models.LogLine {
	if !js.bufferFull {
		result := make([]models.LogLine, js.bufferIdx)
		copy(result, js.buffer[:js.bufferIdx])
		return result
	}

	// Buffer has wrapped: read from bufferIdx to end, then 0 to bufferIdx
	result := make([]models.LogLine, len(js.buffer))
	copy(result, js.buffer[js.bufferIdx:])
	copy(result[len(js.buffer)-js.bufferIdx:], js.buffer[:js.bufferIdx])
	return result
}

// isTerminalStatus returns true if the job status is a final state.
func isTerminalStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

// ErrMaxSubscribers is returned when a job has reached its maximum subscriber limit.
var ErrMaxSubscribers = &maxSubscribersError{}

type maxSubscribersError struct{}

func (e *maxSubscribersError) Error() string {
	return "maximum number of subscribers reached for this job"
}
