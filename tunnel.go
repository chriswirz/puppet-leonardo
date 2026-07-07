package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/chriswirz/https-tunnel/tunnelclient"
)

// TunnelConfig exposes this service on a public HTTPS URL through an
// https-tunnel server, without opening a port on this machine. The handler is
// served in process by the tunnel client, so a client that cannot reach this
// machine's network still reaches the service.
//
// Think about what that means before enabling it: the tunnel is public, and
// every generation spends the configured Leonardo account's tokens. Set
// mcp.auth_token at the very least.
type TunnelConfig struct {
	Enabled bool `json:"enabled"`

	// ServerURL is the tunnel control plane, e.g. https://tunnel.example.com.
	ServerURL string `json:"server_url"`

	// APIKey authenticates against that server. Leave it empty to read
	// APIKeyEnv instead, which keeps the key out of the config file.
	APIKey    string `json:"api_key"`
	APIKeyEnv string `json:"api_key_env"`

	// Subdomain asks for a particular label. The server grants it when free and
	// issues a random one otherwise, so read the URL the client logs.
	Subdomain string `json:"subdomain"`

	// SessionID resumes a previous session, keeping its public URL. It is
	// normally left empty and managed through SessionFile.
	SessionID string `json:"session_id"`

	// SessionFile persists the session id the server issues, so a restart
	// reclaims the same URL. Empty means it is written into config.json.
	SessionFile string `json:"session_file"`

	// Only serves the tunnel alone, with no local listener at all.
	Only bool `json:"only"`

	ClientInfo string `json:"client_info"`
}

// APIKeyValue is the key to authenticate with, from the config or the
// environment variable it names.
func (t TunnelConfig) APIKeyValue() string {
	if t.APIKey != "" {
		return t.APIKey
	}
	return os.Getenv(t.APIKeyEnvName())
}

// APIKeyEnvName is the environment variable the key is read from.
func (t TunnelConfig) APIKeyEnvName() string {
	if t.APIKeyEnv != "" {
		return t.APIKeyEnv
	}
	return "TUNNEL_API_KEY"
}

// validate checks an enabled tunnel has what it needs to connect.
func (t TunnelConfig) validate() error {
	if !t.Enabled {
		return nil
	}
	if t.ServerURL == "" {
		return fmt.Errorf("tunnel.server_url: required when the tunnel is enabled, e.g. https://tunnel.example.com")
	}
	u, err := url.Parse(t.ServerURL)
	if err != nil {
		return fmt.Errorf("tunnel.server_url %q: %w", t.ServerURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("tunnel.server_url: want an http(s) URL, got %q", t.ServerURL)
	}
	if t.APIKeyValue() == "" {
		return fmt.Errorf("tunnel.api_key: required when the tunnel is enabled, or set %s in the environment", t.APIKeyEnvName())
	}
	return nil
}

// Tunnel serves the HTTP handler on a public HTTPS URL through an
// https-tunnel server.
type Tunnel struct {
	client *tunnelclient.Client

	mu  sync.Mutex
	url string
}

// NewTunnel prepares the client. It opens no connection; call Run for that.
func NewTunnel(c *Config, handler http.Handler) (*Tunnel, error) {
	t := &Tunnel{}
	logger := log.Default()

	// The tunnel client logs through slog; route it to the standard logger so
	// its lines look like everything else this server prints.
	slogger := slog.New(slog.NewTextHandler(logWriter{logger}, &slog.HandlerOptions{Level: slog.LevelInfo}))

	clientInfo := c.Tunnel.ClientInfo
	if clientInfo == "" {
		clientInfo = appName + " " + appVersion
	}
	client, err := tunnelclient.New(tunnelclient.Options{
		APIKey:           c.Tunnel.APIKeyValue(),
		ServerURL:        c.Tunnel.ServerURL,
		SessionID:        tunnelSession(c.Tunnel),
		SubdomainRequest: c.Tunnel.Subdomain,
		Handler:          handler,
		Logger:           slogger,
		ClientInfo:       clientInfo,
		OnSession:        sessionSaver(c, logger),
		OnConnect: func(tun tunnelclient.Tunnel) {
			t.mu.Lock()
			t.url = tun.URL
			t.mu.Unlock()
			logger.Printf("tunnel up: %s (MCP endpoint: %s/mcp)", tun.URL, strings.TrimRight(tun.URL, "/"))
		},
	})
	if err != nil {
		return nil, err
	}
	t.client = client
	return t, nil
}

// Run keeps the tunnel up until ctx is cancelled, reconnecting on its own.
func (t *Tunnel) Run(ctx context.Context) error {
	if err := t.client.Run(ctx); err != nil {
		return fmt.Errorf("tunnel: %w", err)
	}
	return nil
}

// URL is the public address, or "" before the first connection succeeds.
func (t *Tunnel) URL() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.url
}

// sessionSaver builds the callback the tunnel client invokes with each session
// id it is issued. That id is what reclaims the same public URL after a
// restart. It goes to the session file when one is named, otherwise into
// config.json. Nothing here fails the tunnel: a tunnel that works but will
// come back on a different URL is worth a warning, not a refusal to serve.
func sessionSaver(c *Config, logger *log.Logger) func(string) error {
	return func(id string) error {
		if file := c.Tunnel.SessionFile; file != "" {
			if err := os.WriteFile(file, []byte(id+"\n"), 0o600); err != nil {
				logger.Printf("tunnel: could not write the session id to %s: %v", file, err)
			} else {
				logger.Printf("tunnel: session id saved to %s; a restart will reclaim this URL", file)
			}
			return nil
		}
		if err := c.SaveSessionID(id); err != nil {
			logger.Printf("tunnel: could not write the session id to %s: %v", c.path, err)
		} else {
			logger.Printf("tunnel: session id saved to %s; a restart will reclaim this URL", c.path)
		}
		return nil
	}
}

// tunnelSession is the session id to resume: the configured one, or whatever
// the last run persisted. An explicitly configured id wins over the file.
func tunnelSession(t TunnelConfig) string {
	if t.SessionID != "" {
		return t.SessionID
	}
	if t.SessionFile == "" {
		return ""
	}
	data, err := os.ReadFile(t.SessionFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// configWriteMu serialises the read-modify-write of the config file.
var configWriteMu sync.Mutex

// SaveSessionID writes a session id the tunnel server issued back into the
// tunnel section of the config file. Every other key keeps its value, and the
// file is written through a temporary file so an interrupted write cannot
// truncate the configuration.
func (c *Config) SaveSessionID(id string) error {
	if id == "" || id == c.Tunnel.SessionID {
		return nil
	}
	c.Tunnel.SessionID = id

	configWriteMu.Lock()
	defer configWriteMu.Unlock()

	raw, err := os.ReadFile(c.path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}), &doc); err != nil {
		return fmt.Errorf("%s: %w", c.path, err)
	}
	section := map[string]json.RawMessage{}
	if b, ok := doc["tunnel"]; ok {
		if err := json.Unmarshal(b, &section); err != nil {
			return fmt.Errorf("%s: tunnel: %w", c.path, err)
		}
	}
	if section["session_id"], err = json.Marshal(id); err != nil {
		return err
	}
	if doc["tunnel"], err = json.Marshal(section); err != nil {
		return err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// 0600 because the config holds the Leonardo password and the tunnel key.
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// logWriter adapts a *log.Logger to io.Writer for slog, dropping the trailing
// newline slog writes so lines are not doubled.
type logWriter struct{ logger *log.Logger }

func (w logWriter) Write(p []byte) (int, error) {
	w.logger.Print(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
