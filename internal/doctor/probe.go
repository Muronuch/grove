package doctor

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Muronuch/grove/internal/router"
)

type HTTPResult struct {
	Status      int
	Body        string
	RouterError bool
}

func probeHTTP(ctx context.Context, routerPort int, host, path string, timeout time.Duration) (HTTPResult, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", routerPort, path), nil)
	if err != nil {
		return HTTPResult{}, err
	}
	req.Host = host
	req.Header.Set("Accept", "*/*")

	client := &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Do(req)
	if err != nil {
		return HTTPResult{}, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	return HTTPResult{
		Status:      res.StatusCode,
		Body:        string(body),
		RouterError: res.Header.Get(router.ErrorHeader) != "",
	}, nil
}

func probeWebSocket(ctx context.Context, routerPort int, host, path, subprotocol string, timeout time.Duration) error {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	deadline := time.Now().Add(timeout)

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", routerPort))
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(nonce)

	proto := ""
	if subprotocol != "" {
		proto = "Sec-WebSocket-Protocol: " + subprotocol + "\r\n"
	}
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n%s\r\n",
		path, host, key, proto)

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("no response to the upgrade request: %w", err)
	}
	if !strings.Contains(status, " 101") {
		rest, _ := io.ReadAll(io.LimitReader(br, 2<<10))
		return fmt.Errorf("expected 101 Switching Protocols, got %q%s",
			strings.TrimSpace(status), summarise(string(rest)))
	}

	headers := map[string]string{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("truncated upgrade response: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if !strings.EqualFold(headers["upgrade"], "websocket") {
		return fmt.Errorf("the response did not carry Upgrade: websocket")
	}

	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if want := base64.StdEncoding.EncodeToString(sum[:]); headers["sec-websocket-accept"] != want {
		return fmt.Errorf("Sec-WebSocket-Accept was %q, expected %q", headers["sec-websocket-accept"], want)
	}
	return nil
}

func summarise(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	return ": " + body
}
