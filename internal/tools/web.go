package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/html"
)

const maxFetchBytes = 2 << 20

type searchArgs struct {
	Query string `json:"query"`
}

type fetchArgs struct {
	URL string `json:"url"`
}

// SearchResult is one web search hit.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// Searcher runs web searches.
type Searcher interface {
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

// Kagi searches with the Kagi Search API (v1).
type Kagi struct {
	Key     string
	BaseURL string
	Client  *http.Client
}

// Search returns up to ten web results.
func (k *Kagi) Search(ctx context.Context, query string) ([]SearchResult, error) {
	base := k.BaseURL
	if base == "" {
		base = "https://kagi.com"
	}
	body, _ := json.Marshal(map[string]any{"query": query, "limit": 10})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/api/v1/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Content-Type", "application/json")
	client := k.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web search failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data *struct {
			Search []struct {
				URL     string `json:"url"`
				Title   string `json:"title"`
				Snippet string `json:"snippet"`
			} `json:"search"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("web search returned HTTP %d with an unreadable body", resp.StatusCode)
	}
	if len(parsed.Errors) > 0 {
		return nil, fmt.Errorf("web search failed: %s", parsed.Errors[0].Message)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("web search returned HTTP %d", resp.StatusCode)
	}
	var out []SearchResult
	if parsed.Data != nil {
		for _, d := range parsed.Data.Search {
			out = append(out, SearchResult{Title: d.Title, URL: d.URL, Snippet: d.Snippet})
		}
	}
	return out, nil
}

func (s *Set) webSearch(ctx context.Context, a searchArgs) (string, error) {
	if strings.TrimSpace(a.Query) == "" {
		return "", errors.New("query is required")
	}
	if s.o.Searcher == nil {
		return "", errors.New("web search is not configured: set the API key named by search.api_key_env in config.toml (KAGI_API_KEY by default)")
	}
	results, err := s.o.Searcher.Search(ctx, a.Query)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "No results.", nil
	}
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", strings.TrimSpace(r.Snippet))
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

var blockedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range []string{
		"0.0.0.0/8",      // "this" network
		"100.64.0.0/10",  // carrier-grade NAT
		"192.0.0.0/24",   // IETF protocol assignments
		"198.18.0.0/15",  // benchmarking
		"240.0.0.0/4",    // reserved
		"64:ff9b::/96",   // NAT64, wraps IPv4 addresses
		"64:ff9b:1::/48", // local-use NAT64
		"2002::/16",      // 6to4, wraps IPv4 addresses
		"2001::/32",      // Teredo
		"fc00::/7",       // unique local
	} {
		_, n, _ := net.ParseCIDR(cidr)
		out = append(out, n)
	}
	return out
}()

// publicIP reports whether ip is a routable public address.
func publicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// SafeHTTPClient refuses connections to local and private addresses, so a
// task with network access cannot reach services on the machine or the LAN.
// The check runs on the address actually dialed, after DNS resolution.
func SafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !publicIP(ip) {
				return fmt.Errorf("fetching local or private addresses (%s) is not allowed", host)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to a non-http URL")
			}
			return nil
		},
	}
}

func (s *Set) webFetch(ctx context.Context, a fetchArgs) (string, error) {
	u, err := url.Parse(a.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("url must be an http or https URL, got %q", a.URL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "autobot/2 (+https://github.com/jgabor/autobot)")
	req.Header.Set("Accept", "text/html,text/plain,application/json;q=0.9,*/*;q=0.5")
	resp, err := s.o.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	cut := len(body) > maxFetchBytes
	if cut {
		body = body[:maxFetchBytes]
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	var text string
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		text = htmlText(string(body))
	case strings.HasPrefix(mediaType, "text/"), mediaType == "application/json",
		strings.HasSuffix(mediaType, "+json"), strings.HasSuffix(mediaType, "+xml"), mediaType == "application/xml", mediaType == "":
		if isBinary(body) {
			return "", errors.New("the response is binary, not text")
		}
		text = string(body)
	default:
		return "", fmt.Errorf("unsupported content type %q", mediaType)
	}
	header := fmt.Sprintf("URL: %s\nStatus: %d\n\n", resp.Request.URL, resp.StatusCode)
	if cut {
		text += "\n[page cut at 2 MiB]"
	}
	return header + strings.TrimSpace(text), nil
}

// htmlText extracts readable text from HTML.
func htmlText(doc string) string {
	z := html.NewTokenizer(strings.NewReader(doc))
	var b strings.Builder
	skip := 0
	block := map[string]bool{
		"p": true, "div": true, "br": true, "li": true, "tr": true, "h1": true, "h2": true, "h3": true,
		"h4": true, "h5": true, "h6": true, "section": true, "article": true, "header": true, "footer": true,
		"pre": true, "blockquote": true, "table": true, "ul": true, "ol": true, "title": true,
	}
	hidden := map[string]bool{"script": true, "style": true, "noscript": true, "svg": true, "template": true, "iframe": true}
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return collapse(b.String())
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			if hidden[string(name)] && tt == html.StartTagToken {
				skip++
			}
			if block[string(name)] {
				b.WriteString("\n")
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if hidden[string(name)] && skip > 0 {
				skip--
			}
			if block[string(name)] {
				b.WriteString("\n")
			}
		case html.TextToken:
			if skip == 0 {
				b.Write(z.Text())
			}
		}
	}
}

func collapse(s string) string {
	var out []string
	blank := false
	for _, line := range strings.Split(s, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
