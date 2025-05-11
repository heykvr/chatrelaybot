package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)


type fakeSlackClient struct {
	messages []string
	calls    int32
}

func (f *fakeSlackClient) PostMessageContext(ctx context.Context, channel string, options ...slack.MsgOption) (string, string, error) {
	atomic.AddInt32(&f.calls, 1)
	f.messages = append(f.messages, "message sent")
	return "", "", nil
}



func TestWorkerPool_SubmitAndShutdown(t *testing.T) {
	var count int32
	pool := NewWorkerPool(3)
	for i := 0; i < 10; i++ {
		pool.Submit(func() {
			atomic.AddInt32(&count, 1)
		})
	}
	pool.Shutdown()
	if count != 10 {
		t.Errorf("expected 10 tasks to run, got %d", count)
	}
}

func TestWorkerPool_ConcurrentSubmit(t *testing.T) {
	var count int32
	pool := NewWorkerPool(5)
	for i := 0; i < 50; i++ {
		pool.Submit(func() {
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&count, 1)
		})
	}
	pool.Shutdown()
	if count != 50 {
		t.Errorf("expected 50 tasks to run, got %d", count)
	}
}


func TestProcessMention_SubmitsTaskForValidQuery(t *testing.T) {
	// Setup test backend
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatResponse{Full: "Test backend reply."})
	}))
	defer ts.Close()
	config.BackendURL = ts.URL

	api := &fakeSlackClient{}
	pool := NewWorkerPool(1)
	defer pool.Shutdown()

	ev := slackevents.AppMentionEvent{
		User:    "U123",
		Channel: "C123",
		BotID:   "B456",
		Text:    "<@B456>   What is Go?",
	}

	processMention(context.Background(), api, ev, pool)
	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&api.calls) == 0 {
		t.Error("expected processTask (via PostMessageContext) to be called")
	}
}

func TestProcessMention_DoesNotSubmitForEmptyQuery(t *testing.T) {
	api := &fakeSlackClient{}
	pool := NewWorkerPool(1)
	defer pool.Shutdown()

	ev := slackevents.AppMentionEvent{
		User:    "U123",
		Channel: "C123",
		BotID:   "B456",
		Text:    "<@B456>   ",
	}

	processMention(context.Background(), api, ev, pool)
	time.Sleep(10 * time.Millisecond)
	if atomic.LoadInt32(&api.calls) != 0 {
		t.Error("processTask should not be called for empty query")
	}
}



func TestProcessTask_JSONResponse(t *testing.T) {
	// Mock backend server returns JSON
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatResponse{
			Full: "Sentence one. Sentence two.",
		})
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	config.BackendURL = ts.URL
	api := &fakeSlackClient{}
	ev := slackevents.AppMentionEvent{
		User:    "U1",
		Channel: "C1",
		Text:    "foo",
	}
	processTask(context.Background(), api, ev, "foo")
	time.Sleep(10 * time.Millisecond)
	if len(api.messages) == 0 {
		t.Errorf("expected at least one message to be sent, got %v", api.messages)
	}
}

func TestProcessTask_SSEResponse(t *testing.T) {
	// Mock backend server returns SSE
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		responses := []ChatResponse{
			{ID: 1, Event: "message_part", Text: "part1"},
			{ID: 2, Event: "message_part", Text: "part2"},
			{ID: 3, Event: "stream_end", Status: "done"},
		}
		for _, resp := range responses {
			data, _ := json.Marshal(resp)
			w.Write([]byte("data: " + string(data) + "\n\n"))
			flusher.Flush()
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	config.BackendURL = ts.URL
	api := &fakeSlackClient{}
	ev := slackevents.AppMentionEvent{
		User:    "U1",
		Channel: "C1",
		Text:    "foo",
	}
	processTask(context.Background(), api, ev, "foo")
	time.Sleep(10 * time.Millisecond)
	if len(api.messages) == 0 {
		t.Errorf("expected at least one message to be sent, got %v", api.messages)
	}
}



func TestProcessDirectMessage_ValidDM(t *testing.T) {
	// 1. Setup test backend
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatResponse{Full: "Test backend reply."})
	}))
	defer ts.Close()
	config.BackendURL = ts.URL

	// 2. Setup fake Slack client and pool
	api := &fakeSlackClient{}
	pool := NewWorkerPool(1)
	defer pool.Shutdown()

	// 3. Create a DM event
	ev := &slackevents.MessageEvent{
		User:        "U1",
		Text:        "hello",
		Channel:     "C1",
		ChannelType: "im",
	}

	// 4. Call the function under test
	processDirectMessage(context.Background(), api, ev, pool)
	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&api.calls) == 0 {
		t.Error("expected message to be sent for valid DM")
	}
}

func TestProcessDirectMessage_IgnoresBotOrNonIM(t *testing.T) {
	api := &fakeSlackClient{}
	pool := NewWorkerPool(1)
	defer pool.Shutdown()
	ev := &slackevents.MessageEvent{
		User:        "U1",
		BotID:       "B1",
		Text:        "hello",
		Channel:     "C1",
		ChannelType: "im",
	}
	processDirectMessage(context.Background(), api, ev, pool)
	time.Sleep(10 * time.Millisecond)
	if atomic.LoadInt32(&api.calls) != 0 {
		t.Error("should not send message for bot")
	}
	ev = &slackevents.MessageEvent{
		User:        "U1",
		Text:        "hello",
		Channel:     "C1",
		ChannelType: "channel",
	}
	processDirectMessage(context.Background(), api, ev, pool)
	time.Sleep(10 * time.Millisecond)
	if atomic.LoadInt32(&api.calls) != 0 {
		t.Error("should not send message for non-IM")
	}
}



func TestBackendRequestFormatting(t *testing.T) {
	ev := slackevents.AppMentionEvent{
		User:    "U1",
		Channel: "C1",
		Text:    "Hello",
	}
	query := "Hello"
	reqBody, _ := json.Marshal(ChatRequest{
		UserID:    ev.User,
		Query:     query,
		ChannelID: ev.Channel,
	})
	var req ChatRequest
	if err := json.Unmarshal(reqBody, &req); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if req.UserID != "U1" || req.Query != "Hello" || req.ChannelID != "C1" {
		t.Errorf("unexpected request formatting: %+v", req)
	}
}



func TestMockBackend_JSONResponse(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatResponse{
			Full: "Complete response to 'foo': Goroutines enable concurrency in Go",
		})
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	body := `{"user_id":"U1","query":"foo","channel_id":"C1"}`
	req, _ := http.NewRequest("POST", ts.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http request failed: %v", err)
	}
	defer resp.Body.Close()
	var chatResp ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if !strings.Contains(chatResp.Full, "Goroutines enable concurrency") {
		t.Errorf("unexpected response: %+v", chatResp)
	}
}

func TestMockBackend_SSEResponse(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		responses := []ChatResponse{
			{ID: 1, Event: "message_part", Text: "Processing: foo"},
			{ID: 2, Event: "message_part", Text: "Goroutines are lightweight threads"},
			{ID: 3, Event: "stream_end", Status: "done"},
		}
		for _, resp := range responses {
			data, _ := json.Marshal(resp)
			w.Write([]byte("data: " + string(data) + "\n\n"))
			flusher.Flush()
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	body := `{"user_id":"U1","query":"foo","channel_id":"C1"}`
	req, _ := http.NewRequest("POST", ts.URL, strings.NewReader(body))
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http request failed: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(bodyBytes), "Goroutines are lightweight threads") {
		t.Error("expected SSE stream to contain 'Goroutines are lightweight threads'")
	}
}
