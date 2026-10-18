// Package wecom implements the enterprise-wechat (企业微信) group-bot
// notification backend. It POSTs markdown messages to the configured webhook
// URL.
package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/angryang/ai-monitor/internal/notifier"
)

// Config is the WeCom backend configuration.
type Config struct {
	WebhookURL string
	Timeout    time.Duration
	HTTPClient *http.Client
}

// Notifier is the WeCom implementation of notifier.Notifier.
type Notifier struct {
	cfg Config
}

// New constructs a WeCom notifier.
func New(cfg Config) *Notifier {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.WebhookURL == "" {
		// Force a sentinel error path (used in tests).
		cfg.WebhookURL = "http://localhost"
	}
	return &Notifier{cfg: cfg}
}

// Name returns the channel name.
func (n *Notifier) Name() string { return "wecom" }

// Send POSTs a markdown body to the webhook.
func (n *Notifier) Send(ctx context.Context, evt notifier.AlertEvent) error {
	body := n.render(evt)
	ctx, cancel := context.WithTimeout(ctx, n.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("wecom: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("wecom: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		// 5xx is retryable; surface as error so callers can decide.
		return fmt.Errorf("wecom: 5xx response: %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		// 4xx: render error class but still attempt to drain the body.
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, resp.Body)
		return fmt.Errorf("wecom: 4xx response: %d %s", resp.StatusCode, buf.String())
	}
	return nil
}

// render builds the JSON payload posted to the webhook.
func (n *Notifier) render(evt notifier.AlertEvent) []byte {
	// WeChat's group-bot accepts a markdown payload {msgtype:"markdown",
	// markdown:{content: "..."}}. Build that content programmatically so the
	// human-readable notification includes task identifiers + timing.
	content := fmt.Sprintf(
		"**AI Monitor Alert** [%s]\n\n"+
			"- rule: `%s`\n"+
			"- task: `%s` (%s)\n"+
			"- severity: `%s`\n"+
			"- message: %s\n"+
			"- at: %s",
		evt.Severity, evt.Rule, evt.TaskID, evt.TaskName,
		evt.Severity, evt.Message, evt.OccurredAt.Format(time.RFC3339Nano),
	)
	payload := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]any{
			"content": content,
		},
		// also include meta data as named custom fields for downstream filter
		"task_id":   evt.TaskID,
		"rule":      string(evt.Rule),
		"severity":  string(evt.Severity),
		"occurred":  evt.OccurredAt.Format(time.RFC3339Nano),
	}
	out, _ := json.Marshal(payload)
	return out
}
