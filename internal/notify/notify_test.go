package notify

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/jgabor/autobot/internal/config"
)

func TestNtfy(t *testing.T) {
	var title, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		title = r.Header.Get("Title")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	n := FromConfig(config.Notify{Method: "ntfy", NtfyURL: srv.URL + "/topic"}, srv.Client())
	if err := n.Send(context.Background(), "autobot: 1 task(s) ran", "notes: completed"); err != nil {
		t.Fatal(err)
	}
	if title != "autobot: 1 task(s) ran" || body != "notes: completed" {
		t.Fatalf("got %q %q", title, body)
	}
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "forbidden", http.StatusForbidden) }))
	defer fail.Close()
	if err := (Ntfy{URL: fail.URL, Client: fail.Client()}).Send(context.Background(), "t", "b"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("error: %v", err)
	}
}

func TestEmail(t *testing.T) {
	t.Setenv("TEST_SMTP_PASSWORD", "pw")
	var gotAddr, gotFrom string
	var gotTo []string
	var gotMsg string
	e := Email{
		Config: config.Email{Host: "smtp.example.com", Port: 587, Username: "me", PasswordEnv: "TEST_SMTP_PASSWORD", From: "me@example.com", To: []string{"you@example.com"}},
		send: func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
			gotAddr, gotFrom, gotTo, gotMsg = addr, from, to, string(msg)
			if a == nil {
				t.Error("no auth")
			}
			return nil
		},
	}
	if err := e.Send(context.Background(), "autobot: done", "line one\nline two"); err != nil {
		t.Fatal(err)
	}
	if gotAddr != "smtp.example.com:587" || gotFrom != "me@example.com" || gotTo[0] != "you@example.com" ||
		!strings.Contains(gotMsg, "Subject: autobot: done\r\n") || !strings.Contains(gotMsg, "line one\r\nline two") {
		t.Fatalf("sent %q to %v via %s:\n%s", gotFrom, gotTo, gotAddr, gotMsg)
	}
	t.Setenv("TEST_SMTP_PASSWORD", "")
	if err := e.Send(context.Background(), "t", "b"); err == nil {
		t.Fatal("empty password: want error")
	}
}

func TestNone(t *testing.T) {
	if FromConfig(config.Notify{Method: "none"}, nil) != nil {
		t.Fatal("none must return nil")
	}
}

// fakeSMTP speaks just enough SMTP for one message and records it.
func fakeSMTP(t *testing.T, silent bool) (string, chan string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if silent {
			time.Sleep(3 * time.Second)
			return
		}
		r := bufio.NewReader(conn)
		say := func(s string) { conn.Write([]byte(s + "\r\n")) }
		say("220 fake")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					got <- data.String()
					say("250 ok")
					continue
				}
				data.WriteString(line)
				continue
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 fake")
			case cmd == "DATA":
				inData = true
				say("354 go ahead")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestSendMail(t *testing.T) {
	addr, got := fakeSMTP(t, false)
	err := sendMail(context.Background(), addr, "127.0.0.1", nil, "me@example.com", []string{"you@example.com"}, []byte("Subject: hi\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if msg := <-got; !strings.Contains(msg, "Subject: hi") {
		t.Fatalf("message: %q", msg)
	}
}

func TestSendMailTimesOut(t *testing.T) {
	addr, _ := fakeSMTP(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := sendMail(ctx, addr, "127.0.0.1", nil, "a@b", []string{"c@d"}, []byte("x")); err == nil {
		t.Fatal("want timeout error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("sendMail ignored its deadline")
	}
}
