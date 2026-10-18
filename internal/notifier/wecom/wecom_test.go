package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/angryang/ai-monitor/internal/notifier"
)

func TestWeCom_Send_Success(t *testing.T) {
	captured := struct {
		method  string
		path    string
		payload map[string]any
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured.payload)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()
	n := New(Config{WebhookURL: srv.URL})
	evt := notifier.AlertEvent{
		TaskID:     "t-1",
		TaskName:   "prod-openai",
		Protocol:   "openai",
		Rule:       notifier.RuleConsecFailure,
		Severity:   notifier.SeverityWarning,
		Message:    "3 consecutive failures",
		OccurredAt: time.Now().UTC(),
	}
	if err := n.Send(context.Background(), evt); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if captured.method != http.MethodPost {
		t.Fatalf("method: %q", captured.method)
	}
	// Verify payload fields are populated (json path → dict lookups).
	if captured.payload["msgtype"] != "markdown" {
		t.Fatalf("msgtype: got %v", captured.payload["msgtype"])
	}
	if captured.payload["task_id"] != "t-1" {
		t.Fatalf("task_id: got %v", captured.payload["task_id"])
	}
	if !strings.Contains(string(jsonOrPanic(captured.payload)), "AI Monitor Alert") {
		t.Fatalf("markdown missing")
	}
}

func TestWeCom_Send_RetryableError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	n := New(Config{WebhookURL: srv.URL})
	err := n.Send(context.Background(), notifier.AlertEvent{TaskID: "t-1", Rule: notifier.RuleConsecFailure})
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if !strings.Contains(err.Error(), "5xx") {
		t.Fatalf("err: %v", err)
	}
}

func TestWeCom_Send_ClientError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "invalid webhook")
	}))
	defer srv.Close()
	n := New(Config{WebhookURL: srv.URL})
	err := n.Send(context.Background(), notifier.AlertEvent{TaskID: "t-1"})
	if err == nil {
		t.Fatal("expected error for 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("err: %v", err)
	}
}

func TestWeCom_Send_NetworkError(t *testing.T) {
	n := New(Config{WebhookURL: "http://invalid-host:9999"})
	err := n.Send(context.Background(), notifier.AlertEvent{TaskID: "t-1"})
	if err == nil {
		t.Fatal("expected error for unreachable host")
	}
	if !errors.Is(err, err) {
		t.Logf("got error: %v", err)
	}
}

func jsonOrPanic(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
