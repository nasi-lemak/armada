package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// notifyJob is one notification for one channel.
type notifyJob struct {
	ch *api.NotificationChannel
	n  api.AlertNotification
}

// notifyLoop delivers notifications in the background with retries, so a
// slow or broken endpoint never blocks the API or the rollout engine.
func (s *Server) notifyLoop() {
	for j := range s.notifyQueue {
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt*attempt) * 2 * time.Second)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			err = s.sendNotification(ctx, j.ch, j.n)
			cancel()
			if err == nil {
				break
			}
		}
		if err != nil {
			s.log.Warn("alert notification failed", "channel", j.ch.Name, "alert", j.n.AlertID, "err", err)
			s.logEvent(context.Background(), "alert.delivery_failed", j.n.DeviceID, j.n.DeploymentID,
				fmt.Sprintf("could not notify %s about %q: %v", j.ch.Name, j.n.Rule, err))
		}
	}
}

func (s *Server) enqueueNotification(ch *api.NotificationChannel, n api.AlertNotification) {
	select {
	case s.notifyQueue <- notifyJob{ch, n}:
	default:
		s.log.Error("notification queue full; dropping", "channel", ch.Name, "alert", n.AlertID)
	}
}

func (s *Server) sendNotification(ctx context.Context, ch *api.NotificationChannel, n api.AlertNotification) error {
	client := !scopeEmpty(ch.ClientScope)
	if client {
		ctx = withClientChannel(ctx)
	}
	switch ch.Type {
	case "webhook":
		return s.sendWebhook(ctx, ch.Webhook, n)
	case "slack":
		return s.sendSlack(ctx, ch.Slack, n)
	case "email":
		e := ch.Email
		if e == nil {
			return errors.New("email channel has no configuration")
		}
		if client || e.Host == "" {
			// Through the vendor's relay; clients only choose recipients.
			if s.cfg.SMTPRelay.Host == "" {
				return errors.New("no SMTP relay is configured on the server (-smtp-host)")
			}
			relay := s.cfg.SMTPRelay
			relay.To = e.To
			e = &relay
		}
		return sendEmail(ctx, e, n)
	}
	return fmt.Errorf("unknown channel type %q", ch.Type)
}

var templateFuncs = template.FuncMap{
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
	"upper": strings.ToUpper,
	"lower": strings.ToLower,
}

// renderTemplate executes a webhook body template against a notification.
func renderTemplate(tpl string, n api.AlertNotification) ([]byte, error) {
	t, err := template.New("body").Funcs(templateFuncs).Option("missingkey=error").Parse(tpl)
	if err != nil {
		return nil, fmt.Errorf("body template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, n); err != nil {
		return nil, fmt.Errorf("body template: %w", err)
	}
	return buf.Bytes(), nil
}

func (s *Server) sendWebhook(ctx context.Context, c *api.WebhookConfig, n api.AlertNotification) error {
	if c == nil {
		return errors.New("webhook channel has no configuration")
	}
	var body []byte
	var err error
	if c.BodyTemplate != "" {
		body, err = renderTemplate(c.BodyTemplate, n)
	} else {
		body, err = json.Marshal(n)
	}
	if err != nil {
		return err
	}
	method := c.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "armada-server")
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	if c.Secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(c.Secret))
		mac.Write([]byte(ts + "."))
		mac.Write(body)
		req.Header.Set("X-Armada-Timestamp", ts)
		req.Header.Set("X-Armada-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	return s.doNotifyRequest(req)
}

func (s *Server) doNotifyRequest(req *http.Request) error {
	cl := s.notifyClient
	if isClientChannel(req.Context()) {
		cl = s.clientNotifyClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func statusIcon(n api.AlertNotification) string {
	switch {
	case n.Status == api.AlertResolved:
		return "✅"
	case n.Status == "test":
		return "🔔"
	case n.Severity == api.HealthCritical:
		return "🔴"
	}
	return "🟠"
}

// notificationTitle is a one-line headline, e.g. "🔴 CRITICAL: Disk failing — kiosk-07".
func notificationTitle(n api.AlertNotification) string {
	label := strings.ToUpper(n.Severity)
	if n.Status == api.AlertResolved {
		label = "RESOLVED"
	}
	if n.Status == "test" {
		label = "TEST"
	}
	t := fmt.Sprintf("%s %s: %s", statusIcon(n), label, n.Rule)
	if n.DeviceName != "" {
		t += " — " + n.DeviceName
	}
	return t
}

func (s *Server) sendSlack(ctx context.Context, c *api.SlackConfig, n api.AlertNotification) error {
	if c == nil || c.WebhookURL == "" {
		return errors.New("slack channel has no webhook URL")
	}
	text := "*" + notificationTitle(n) + "*\n" + n.Summary
	if n.Count > 1 {
		text += fmt.Sprintf(" (%d occurrences)", n.Count)
	}
	if n.URL != "" {
		text += "\n<" + n.URL + "|Open in Armada>"
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return s.doNotifyRequest(req)
}

func sendEmail(ctx context.Context, c *api.EmailConfig, n api.AlertNotification) error {
	if c == nil || c.Host == "" || c.From == "" || len(c.To) == 0 {
		return errors.New("email channel needs host, from and to")
	}
	port := c.Port
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(port))
	subject := strings.TrimSpace(strings.TrimLeft(notificationTitle(n), "🔴🟠✅🔔 "))
	var body strings.Builder
	fmt.Fprintf(&body, "%s\r\n\r\n", n.Summary)
	if n.DeviceName != "" {
		fmt.Fprintf(&body, "Device:   %s (%s)\r\n", n.DeviceName, n.DeviceID)
	}
	if n.DeploymentID != "" {
		fmt.Fprintf(&body, "Rollout:  %s\r\n", n.DeploymentID)
	}
	fmt.Fprintf(&body, "Severity: %s\r\nStatus:   %s\r\nSince:    %s\r\n", n.Severity, n.Status, n.StartedAt.Format(time.RFC1123))
	if n.Count > 1 {
		fmt.Fprintf(&body, "Count:    %d\r\n", n.Count)
	}
	if n.URL != "" {
		fmt.Fprintf(&body, "\r\n%s\r\n", n.URL)
	}
	msg := "From: " + c.From + "\r\nTo: " + strings.Join(c.To, ", ") + "\r\nSubject: " + mime.QEncoding.Encode("utf-8", "[Armada] "+subject) +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\n" + body.String()

	d := net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if port == 465 {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer cl.Close()
	if ok, _ := cl.Extension("STARTTLS"); ok && port != 465 {
		if err := cl.StartTLS(&tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if c.Username != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return err
		}
	}
	if err := cl.Mail(c.From); err != nil {
		return err
	}
	for _, to := range c.To {
		if err := cl.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}
