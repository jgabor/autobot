// Package notify tells the user a run finished.
package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jgabor/autobot/internal/config"
)

// Notifier sends one message.
type Notifier interface {
	Send(ctx context.Context, title, body string) error
}

// FromConfig returns the configured notifier, or nil for "none".
func FromConfig(c config.Notify, client *http.Client) Notifier {
	switch c.Method {
	case "notify-send":
		return NotifySend{}
	case "ntfy":
		if client == nil {
			client = &http.Client{Timeout: 30 * time.Second}
		}
		return Ntfy{URL: c.NtfyURL, Client: client}
	case "email":
		return Email{Config: c.Email}
	}
	return nil
}

// NotifySend shows a desktop notification.
type NotifySend struct{}

func (NotifySend) Send(ctx context.Context, title, body string) error {
	out, err := exec.CommandContext(ctx, "notify-send", "--app-name=autobot", title, body).CombinedOutput()
	if err != nil {
		return fmt.Errorf("notify-send: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Ntfy posts to an ntfy topic URL.
type Ntfy struct {
	URL    string
	Client *http.Client
}

func (n Ntfy) Send(ctx context.Context, title, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", "robot")
	resp, err := n.Client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("ntfy returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Email sends mail through SMTP, upgrading to TLS when the server offers
// STARTTLS.
type Email struct {
	Config config.Email
	// send is replaced in tests.
	send func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

func (e Email) Send(ctx context.Context, title, body string) error {
	c := e.Config
	var auth smtp.Auth
	if c.Username != "" {
		password := ""
		if c.PasswordEnv != "" {
			password = os.Getenv(c.PasswordEnv)
		}
		if password == "" {
			return errors.New("email: the password environment variable is empty")
		}
		auth = smtp.PlainAuth("", c.Username, password, c.Host)
	}
	msg := "From: " + c.From + "\r\n" +
		"To: " + strings.Join(c.To, ", ") + "\r\n" +
		"Subject: " + title + "\r\n" +
		"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		strings.ReplaceAll(body, "\n", "\r\n") + "\r\n"
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	send := e.send
	if send == nil {
		send = func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
			return sendMail(ctx, addr, c.Host, a, from, to, msg)
		}
	}
	if err := send(addr, auth, c.From, c.To, []byte(msg)); err != nil {
		return fmt.Errorf("email: %w", err)
	}
	return nil
}

// sendMail is smtp.SendMail with a deadline, so a stalled server cannot
// hold up the run. It upgrades to TLS when the server offers STARTTLS.
func sendMail(ctx context.Context, addr, host string, a smtp.Auth, from string, to []string, msg []byte) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Minute)
	}
	conn, err := (&net.Dialer{Deadline: deadline}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	conn.SetDeadline(deadline)
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if a != nil {
		if err := client.Auth(a); err != nil {
			return err
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}
