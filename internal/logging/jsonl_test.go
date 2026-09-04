package logging

import (
	"testing"
	"time"
)

func TestEventNeverBlocksWhenQueueIsFull(t *testing.T) {
	logger := &Logger{queue: make(chan logEvent, 1)}
	logger.queue <- logEvent{}

	done := make(chan struct{})
	go func() {
		logger.Event("INFO", "test", nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Event blocked on a full logging queue")
	}
	if logger.dropped.Load() != 1 {
		t.Fatalf("expected one dropped event, got %d", logger.dropped.Load())
	}
}

func TestStreamFloodDoesNotConsumeLifecycleQueue(t *testing.T) {
	logger := &Logger{queue: make(chan logEvent, 1), streamQueue: make(chan logEvent, 1)}
	logger.streamQueue <- logEvent{}
	logger.Event("INFO", "chatgpt_tool_call_stream", map[string]any{"delta": "noise"})
	logger.Event("INFO", "chatgpt_tool_call_succeeded", map[string]any{"status": "completed"})

	select {
	case event := <-logger.queue:
		if event.message != "chatgpt_tool_call_succeeded" {
			t.Fatalf("unexpected lifecycle event: %s", event.message)
		}
	default:
		t.Fatal("lifecycle event was displaced by stream flood")
	}
}
