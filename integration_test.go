package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// Mock backend for integration
func loadBotToken(t *testing.T) string {
	err := godotenv.Load()
	if err != nil {
		t.Fatalf("Error loading .env file: %v", err)
	}
	token := os.Getenv("SLACK_BOT_TOKEN")
	if token == "" {
		t.Fatal("SLACK_BOT_TOKEN not set in environment")
	}
	return token
}

func TestIntegration_ProcessMention_EndToEnd(t *testing.T) {
	// Start a mock backend server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatResponse{Full: "Integration test reply."})
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
	time.Sleep(500 * time.Millisecond)
	if len(api.messages) == 0 {
		t.Error("expected message to be sent for mention")
	}
}

func TestIntegration_ProcessDirectMessage_EndToEnd(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatResponse{Full: "DM integration reply."})
	}))
	t.Log("Backend URL:", ts.URL)
	defer ts.Close()
	config.BackendURL = ts.URL

	api := &fakeSlackClient{}
	pool := NewWorkerPool(1)
	defer pool.Shutdown()

	ev := &slackevents.MessageEvent{
		User:        "U1",
		Text:        "hello",
		Channel:     "C1",
		ChannelType: "im",
	}

	processDirectMessage(context.Background(), api, ev, pool)
	time.Sleep(500 * time.Millisecond)
	if len(api.messages) == 0 {
		t.Error("expected message to be sent for DM")
	}
}

func TestIntegration_MentionWithBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatResponse{Full: "integration reply"})
	}))
	defer backend.Close()
	config.BackendURL = backend.URL

	api := &fakeSlackClient{}
	pool := NewWorkerPool(1)
	defer pool.Shutdown()

	ev := slackevents.AppMentionEvent{
		User:    "U123",
		Channel: "C123",
		BotID:   "B456",
		Text:    "<@B456> What is Go?",
	}
	processMention(context.Background(), api, ev, pool)
	time.Sleep(100 * time.Millisecond)
	if len(api.messages) == 0 {
		t.Error("expected message to be sent for mention")
	}
}
func TestIntegration_DirectMessageWithBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatResponse{Full: "integration reply"})
	}))
	defer backend.Close()
	config.BackendURL = backend.URL

	api := &fakeSlackClient{}
	pool := NewWorkerPool(1)
	defer pool.Shutdown()

	ev := &slackevents.MessageEvent{
		User:        "U1",
		Text:        "hello",
		Channel:     "C1",
		ChannelType: "im",
	}
	processDirectMessage(context.Background(), api, ev, pool)
	time.Sleep(100 * time.Millisecond)
	if len(api.messages) == 0 {
		t.Error("expected message to be sent for DM")
	}
}

func TestProcessTask_BackendError(t *testing.T) {
	config.BackendURL = "http://127.0.0.1:0" // Invalid URL
	api := &fakeSlackClient{}
	ev := slackevents.AppMentionEvent{User: "U1", Channel: "C1", Text: "foo"}
	processTask(context.Background(), api, ev, "foo")
	// Optionally check no message sent
}

func TestProcessTask_SuccessfulResponse(t *testing.T) {
	// Mock backend server returns valid JSON
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatResponse{Full: "valid response"})
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	config.BackendURL = ts.URL
	api := &fakeSlackClient{}
	ev := slackevents.AppMentionEvent{User: "U1", Channel: "C1", Text: "foo"}
	processTask(context.Background(), api, ev, "foo")
	time.Sleep(10 * time.Millisecond)
	if len(api.messages) == 0 {
		t.Error("expected message to be sent for valid response")
	}
}

func TestSendMessageToChannel(t *testing.T) {
	api := &fakeSlackClient{}
	channel := os.Getenv("SLACK_CHANNEL")

	// Simulate sending a message
	ctx := context.Background()
	_, _, err := api.PostMessageContext(ctx, channel, slack.MsgOptionText("Hello, channel!", false))
	t.Log("Message sent to channel:", channel)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(api.messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(api.messages))
	}
}

func TestSendRealMessageToChannel(t *testing.T) {
	token := loadBotToken(t)
	api := slack.New(token)
	channel := os.Getenv("SLACK_CHANNEL")
	text := "Hello from Go integration test!"

	_, _, err := api.PostMessage(
		channel,
		slack.MsgOptionText(text, false),
	)
	if err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}
	t.Log("Message sent to channel:", channel)
	
}

func TestSendMentionToBotInChannel(t *testing.T) {
	token := loadBotToken(t)
	api := slack.New(token)
	channel := os.Getenv("SLACK_CHANNEL")                  
	botUserID := os.Getenv("SLACK_BOT_USER_ID")
	if botUserID == "" {
		botUserID = "chatrelaybot" 
	}
	text := "<@" + botUserID + "> Hello, this is a test mention!"

	_, _, err := api.PostMessage(
		channel,
		slack.MsgOptionText(text, false),
	)
	if err != nil {
		t.Fatalf("Failed to send mention: %v", err)
	}
	t.Log("Mention sent to bot in channel:", channel)
}
