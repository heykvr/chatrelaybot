package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
)

// Configuration
const (
	DefaultPort        = "8080"
	DefaultBackendPath = "/v1/chat/stream"
	MaxWorkers         = 100
)

var config = struct {
	SlackBotToken string
	SlackAppToken string
	BackendURL    string
	OtelEndpoint  string
	Port          string
}{}

// Worker Pool
type WorkerPool struct {
	tasks chan func()
	wg    sync.WaitGroup
}

func NewWorkerPool(maxWorkers int) *WorkerPool {
	pool := &WorkerPool{
		tasks: make(chan func(), maxWorkers*2),
	}
	for i := 0; i < maxWorkers; i++ {
		pool.wg.Add(1)
		go pool.worker()
	}
	return pool
}

func (p *WorkerPool) worker() {
	defer p.wg.Done()
	for task := range p.tasks {
		task()
	}
}

func (p *WorkerPool) Submit(task func()) {
	p.tasks <- task
}

func (p *WorkerPool) Shutdown() {
	close(p.tasks)
	p.wg.Wait()
}

// OpenTelemetry
func initTracer() (*sdktrace.TracerProvider, error) {
	exporter, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("chatrelay-bot"),
			semconv.ServiceVersion("1.0.0"),
			attribute.String("environment", "production"),
		)),
	)
	otel.SetTracerProvider(tp)
	return tp, nil
}

// Backend Mock
type ChatRequest struct {
	UserID    string `json:"user_id"`
	Query     string `json:"query"`
	ChannelID string `json:"channel_id"`
}

type ChatResponse struct {
	ID     int    `json:"id,omitempty"`
	Event  string `json:"event,omitempty"`
	Text   string `json:"text_chunk,omitempty"`
	Status string `json:"status,omitempty"`
	Full   string `json:"full_response,omitempty"`
	Error  string `json:"error,omitempty"`
}

type SlackClient interface {
	PostMessageContext(ctx context.Context, channel string, options ...slack.MsgOption) (string, string, error)
}

func mockBackend() {
	http.HandleFunc(DefaultBackendPath, func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("backend").Start(r.Context(), "handle_request")
		defer span.End()

		logWithTrace(ctx, "Received request to backend")

		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			span.RecordError(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		span.SetAttributes(
			attribute.String("user.id", req.UserID),
			attribute.String("channel.id", req.ChannelID),
			attribute.String("query", req.Query),
		)

		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)

			responses := []ChatResponse{
				{ID: 1, Event: "message_part", Text: fmt.Sprintf("Processing: %s", req.Query)},
				{ID: 2, Event: "message_part", Text: "Goroutines are lightweight threads"},
				{ID: 3, Event: "message_part", Text: "They enable concurrent execution"},
				{ID: 4, Event: "stream_end", Status: "done"},
			}

			for _, resp := range responses {
				data, _ := json.Marshal(resp)
				fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", resp.ID, resp.Event, data)
				flusher.Flush()
				time.Sleep(300 * time.Millisecond)
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ChatResponse{
				Full: fmt.Sprintf("Complete response to '%s': Goroutines enable concurrency in Go", req.Query),
			})
		}
	})

	log.Printf("Backend running on :%s%s", config.Port, DefaultBackendPath)
	log.Fatal(http.ListenAndServe(":"+config.Port, nil))
}

// Bot Logic
func processMention(ctx context.Context, api SlackClient, ev slackevents.AppMentionEvent, pool *WorkerPool) {
	ctx, span := otel.Tracer("bot").Start(ctx, "process_mention")
	defer span.End()

	cleanQuery := strings.TrimSpace(strings.ReplaceAll(ev.Text, "<@"+ev.BotID+">", ""))
	if cleanQuery == "" {
		span.SetAttributes(attribute.Bool("error.invalid_input", true))
		return
	}

	span.SetAttributes(
		attribute.String("user.id", ev.User),
		attribute.String("channel.id", ev.Channel),
		attribute.String("query", cleanQuery),
	)

	logWithTrace(ctx, fmt.Sprintf("Received mention: %s", cleanQuery))

	pool.Submit(func() {
		processTask(ctx, api, ev, cleanQuery)
	})
}

func processTask(ctx context.Context, api SlackClient, ev slackevents.AppMentionEvent, query string) {
	ctx, span := otel.Tracer("bot").Start(ctx, "backend_request")
	defer span.End()

	reqBody, _ := json.Marshal(ChatRequest{
		UserID:    ev.User,
		Query:     query,
		ChannelID: ev.Channel,
	})

	var resp *http.Response
	var err error

	for attempt := 0; attempt < 3; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "POST", config.BackendURL, strings.NewReader(string(reqBody)))
		req.Header.Set("Accept", "text/event-stream")
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			break
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}

	if err != nil {
		span.RecordError(err)
		logWithTrace(ctx, "Failed to reach backend")
		api.PostMessageContext(ctx, ev.Channel, slack.MsgOptionText("Service unavailable, please try later", false))
		return
	}
	defer resp.Body.Close()

	switch resp.Header.Get("Content-Type") {
	case "text/event-stream":
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
				line := scanner.Text()
				if strings.HasPrefix(line, "data: ") {
					var msg ChatResponse
					if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &msg); err == nil {
						if msg.Event == "message_part" {
							api.PostMessageContext(ctx, ev.Channel, slack.MsgOptionText(msg.Text, false))
							time.Sleep(500 * time.Millisecond)
						}
					}
				}
			}
		}
	default:
		var result ChatResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err == nil {
			chunks := strings.SplitAfter(result.Full, ". ")
			for _, chunk := range chunks {
				chunk = strings.TrimSpace(chunk)
				if chunk != "" {
					api.PostMessageContext(ctx, ev.Channel, slack.MsgOptionText(chunk, false))
					time.Sleep(500 * time.Millisecond)
				}
			}
		}
	}
}

// Tracing Logs
func logWithTrace(ctx context.Context, msg string) {
	if span := trace.SpanFromContext(ctx); span != nil {
		sc := span.SpanContext()
		log.Printf("[trace_id=%s span_id=%s] %s", sc.TraceID(), sc.SpanID(), msg)
	} else {
		log.Println(msg)
	}
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: No .env file loaded: %v", err)
	}

	config.SlackBotToken = os.Getenv("SLACK_BOT_TOKEN")
	config.SlackAppToken = os.Getenv("SLACK_APP_TOKEN")
	config.BackendURL = os.Getenv("BACKEND_URL")
	config.OtelEndpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	config.Port = os.Getenv("PORT")
	if config.Port == "" {
		config.Port = DefaultPort
	}

	tp, err := initTracer()
	if err != nil {
		log.Fatalf("Failed to initialize tracer: %v", err)
	}
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			log.Printf("Error shutting down tracer: %v", err)
		}
	}()

	go mockBackend()

	api := slack.New(
		config.SlackBotToken,
		slack.OptionAppLevelToken(config.SlackAppToken),
		slack.OptionDebug(true),
	)

	socket := socketmode.New(
		api,
		socketmode.OptionLog(log.New(os.Stdout, "socketmode: ", log.Lshortfile|log.LstdFlags)),
	)

	pool := NewWorkerPool(MaxWorkers)
	defer pool.Shutdown()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		for evt := range socket.Events {
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				socket.Ack(*evt.Request)
				if eventsAPIEvent.Type == slackevents.CallbackEvent {
					switch innerEvent := eventsAPIEvent.InnerEvent.Data.(type) {
					case *slackevents.AppMentionEvent:
						processMention(ctx, api, *innerEvent, pool)
					case *slackevents.MessageEvent:
						processDirectMessage(ctx, api, innerEvent, pool)
					}
				}
			}
		}
	}()

	log.Println("Starting ChatRelayBot...")
	if err := socket.RunContext(ctx); err != nil {
		log.Fatalf("Socket mode failed: %v", err)
	}
}

func processDirectMessage(ctx context.Context, api SlackClient, ev *slackevents.MessageEvent, pool *WorkerPool) {
	ctx, span := otel.Tracer("bot").Start(ctx, "process_direct_message")
	defer span.End()

	if ev.BotID != "" || ev.Text == "" {
		return
	}

	if ev.ChannelType != "im" {
		return
	}

	span.SetAttributes(
		attribute.String("user.id", ev.User),
		attribute.String("channel.id", ev.Channel),
		attribute.String("query", ev.Text),
	)

	logWithTrace(ctx, fmt.Sprintf("Received DM: %s", ev.Text))

	pool.Submit(func() {
		processTask(ctx, api, slackevents.AppMentionEvent{
			User:    ev.User,
			Channel: ev.Channel,
			Text:    ev.Text,
		}, ev.Text)
	})
}
