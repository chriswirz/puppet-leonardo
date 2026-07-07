package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/playwright-community/playwright-go"
)

const (
	loginURL    = "https://app.leonardo.ai/auth/login"
	generateURL = "https://app.leonardo.ai/generate?model=auto-preset"
)

// humanVerifyTimeoutMs is how long to wait for the Cloudflare "verify you are a
// human" check to clear (auto or by a person ticking the box) before giving up.
const humanVerifyTimeoutMs = 180000

// continueWithEmailSelector matches the "Continue with Email" button across its
// known variants: an id, a data-tracking-id, or plain button text (the label may
// live in a child <span>).
const continueWithEmailSelector = `#login_sign_up_continue_with_email, ` +
	`[data-tracking-id="login_sign_up_continue_with_email"], ` +
	`button:has-text("Continue with Email")`

// Config is loaded from config.json at startup.
type Config struct {
	Port        int    `json:"port"`
	Headless    bool   `json:"headless"`
	DefaultSize string `json:"default_size"`
	ProfileDir  string `json:"profile_dir"`
	StepDelayMs int    `json:"step_delay_ms"`
	Debug       bool   `json:"debug"`
	// TasksFile is the JSON file holding the dictionary of task id (prompt
	// hash) -> task (status, prompt, uri, error, timestamps), so it persists
	// across restarts. It is rewritten when a task is queued, starts, is
	// submitted to Leonardo, completes or fails.
	TasksFile string `json:"tasks_file"`
	// QueueDepth is how many generations may wait behind the running one
	// (default 100). Beyond that a request is rejected; 0 allows no waiting.
	QueueDepth *int `json:"queue_depth"`
	// ClearDirOnStartup, if set, is a directory (e.g. "chrome-cdp") that is
	// deleted when the server starts, so each run begins from a clean profile.
	ClearDirOnStartup string `json:"clear_dir_on_startup"`
	// CDPURL, if set (e.g. "http://localhost:9222"), makes the app attach to an
	// already-running Chrome via the DevTools protocol instead of launching its
	// own browser. This is the reliable way past Cloudflare Turnstile, since the
	// browser is a genuine user session with no automation fingerprints.
	CDPURL   string `json:"cdp_url"`
	Leonardo struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	} `json:"leonardo"`
	// MCP configures the /mcp endpoint.
	MCP MCPConfig `json:"mcp"`
	// Tunnel optionally exposes this service on a public URL (see tunnel.go).
	Tunnel TunnelConfig `json:"tunnel"`

	// path is the file this was loaded from, so a tunnel session id can be
	// written back to it. It is not part of the file.
	path string
}

// MCPConfig covers the /mcp endpoint's access control and protocol options.
type MCPConfig struct {
	// Instructions replaces the default text the server gives MCP clients.
	Instructions string `json:"instructions"`
	// AuthToken, if set, is a bearer token every /mcp request must carry.
	AuthToken string `json:"auth_token"`
	// AllowedOrigins lists the browser Origins allowed to call /mcp (DNS
	// rebinding defence). An entry without a port matches any port; "*"
	// allows any origin. Requests without an Origin are always allowed.
	AllowedOrigins []string `json:"allowed_origins"`
	// AllowedHeaders is what a CORS preflight is told a browser may send.
	// ["*"] allows any header; empty echoes whatever the preflight asks for.
	AllowedHeaders []string `json:"allowed_headers"`
	// AllowPrivateNetwork answers Chrome's Private Network Access preflight.
	AllowPrivateNetwork bool `json:"allow_private_network"`
	// LegacyCompatibility also serves the initialize-based protocol revisions
	// (2025-03-26 through 2025-11-25), which most clients still speak.
	LegacyCompatibility bool `json:"legacy_compatibility"`
	// SessionTimeoutSeconds is how long an idle legacy session is kept.
	SessionTimeoutSeconds int `json:"session_timeout_seconds"`
	// GenerateWaitSeconds is how long generate_image_with_leonardo waits for the
	// image before returning the task as running, when the call does not say.
	// Kept under typical MCP client timeouts; negative waits for completion.
	GenerateWaitSeconds int `json:"generate_wait_seconds"`
}

var cfg Config

// loadConfig reads config.json (path overridable via CONFIG env var) and applies
// defaults for any unset fields.
func loadConfig() error {
	path := os.Getenv("CONFIG")
	if path == "" {
		path = "config.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	// Defaults for booleans that are on unless the file turns them off.
	cfg.MCP.AllowPrivateNetwork = true
	cfg.MCP.LegacyCompatibility = true
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	cfg.path = path
	if cfg.MCP.AllowedOrigins == nil {
		cfg.MCP.AllowedOrigins = []string{"http://127.0.0.1", "http://localhost"}
	}
	if cfg.MCP.SessionTimeoutSeconds == 0 {
		cfg.MCP.SessionTimeoutSeconds = 7200
	}
	if cfg.MCP.GenerateWaitSeconds == 0 {
		cfg.MCP.GenerateWaitSeconds = 45
	}
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	if cfg.DefaultSize == "" {
		cfg.DefaultSize = "16:9"
	}
	if cfg.ProfileDir == "" {
		cfg.ProfileDir = "profile"
	}
	if cfg.TasksFile == "" {
		cfg.TasksFile = "tasks.json"
	}
	if cfg.QueueDepth == nil {
		cfg.QueueDepth = new(int)
		*cfg.QueueDepth = 100
	}
	if *cfg.QueueDepth < 0 {
		return fmt.Errorf("config queue_depth %d must not be negative", *cfg.QueueDepth)
	}
	if !validSizes[cfg.DefaultSize] {
		return fmt.Errorf("config default_size %q must be one of 2:3, 1:1, 16:9", cfg.DefaultSize)
	}
	if cfg.Leonardo.Email == "" || cfg.Leonardo.Password == "" {
		return fmt.Errorf("config leonardo.email and leonardo.password are required")
	}
	return nil
}

// browserSession keeps a single logged-in browser backed by a persistent
// profile directory (so the Leonardo session survives restarts). Requests are
// serialized through the mutex.
type browserSession struct {
	mu   sync.Mutex
	pw   *playwright.Playwright
	br   playwright.Browser // set only in CDP-attach mode
	ctx  playwright.BrowserContext
	page playwright.Page
	// run performs one generation with mu held; nil means generateLocked.
	// Tests replace it to stand in for the browser.
	run func(prompt, size string) (string, error)
}

var session = &browserSession{}

// launch starts playwright and opens a persistent browser context using the
// configured profile directory. headless overrides cfg.Headless (used by the
// interactive -login flow, which is always headed).
func (s *browserSession) launch(headless bool) error {
	s.close()
	pw, err := playwright.Run()
	if err != nil {
		return fmt.Errorf("starting playwright: %w", err)
	}
	s.pw = pw
	if cfg.CDPURL != "" {
		return s.attachCDP()
	}

	opts := playwright.BrowserTypeLaunchPersistentContextOptions{
		Headless: playwright.Bool(headless),
		// Anti-automation-detection: Cloudflare Turnstile flags Playwright's default
		// Chromium via navigator.webdriver, the --enable-automation switch, and the
		// AutomationControlled blink feature. Use real Chrome, drop the automation
		// switches, keep the sandbox (a disabled sandbox shows a warning banner that
		// is itself a tell), and disable that feature so it looks like a normal one.
		Channel:           playwright.String("chrome"),
		ChromiumSandbox:   playwright.Bool(true),
		IgnoreDefaultArgs: []string{"--enable-automation", "--no-sandbox"},
		Args:              []string{"--disable-blink-features=AutomationControlled"},
		UserAgent:         playwright.String("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
	}
	ctx, err := pw.Chromium.LaunchPersistentContext(cfg.ProfileDir, opts)
	if err != nil {
		// Real Chrome may not be installed; fall back to bundled Chromium.
		if cfg.Debug {
			log.Println("[launch] chrome channel failed, falling back to bundled chromium:", err)
		}
		opts.Channel = nil
		ctx, err = pw.Chromium.LaunchPersistentContext(cfg.ProfileDir, opts)
	}
	if err != nil {
		pw.Stop()
		s.pw = nil
		return fmt.Errorf("launching persistent context: %w", err)
	}
	return s.adopt(ctx)
}

// attachCDP connects to an already-running Chrome over the DevTools protocol
// (see cfg.CDPURL). It reuses that browser's existing context and page.
func (s *browserSession) attachCDP() error {
	// Chrome's DevTools endpoint binds to 127.0.0.1 only; "localhost" can resolve
	// to IPv6 ::1 first and be refused. Normalize to the IPv4 loopback.
	cdpURL := strings.Replace(cfg.CDPURL, "localhost", "127.0.0.1", 1)
	log.Println("attaching to existing Chrome over CDP at", cdpURL)
	br, err := s.pw.Chromium.ConnectOverCDP(cdpURL)
	if err != nil {
		s.pw.Stop()
		s.pw = nil
		return fmt.Errorf("connecting to Chrome at %s (start Chrome with --remote-debugging-port and a --user-data-dir): %w", cdpURL, err)
	}
	s.br = br
	var ctx playwright.BrowserContext
	if ctxs := br.Contexts(); len(ctxs) > 0 {
		ctx = ctxs[0]
	} else if ctx, err = br.NewContext(); err != nil {
		s.close()
		return fmt.Errorf("creating context over CDP: %w", err)
	}
	return s.adopt(ctx)
}

// adopt wires a browser context into the session, picking an existing page or
// opening a new one, and installs the webdriver-hiding init script.
func (s *browserSession) adopt(ctx playwright.BrowserContext) error {
	if err := ctx.AddInitScript(playwright.Script{
		Content: playwright.String(`Object.defineProperty(navigator, 'webdriver', {get: () => undefined});`),
	}); err != nil && cfg.Debug {
		log.Println("[launch] AddInitScript failed:", err)
	}
	var (
		page playwright.Page
		err  error
	)
	if pages := ctx.Pages(); len(pages) > 0 {
		page = pages[0]
	} else if page, err = ctx.NewPage(); err != nil {
		s.close()
		return fmt.Errorf("opening page: %w", err)
	}
	s.ctx, s.page = ctx, page
	return nil
}

func (s *browserSession) ensure() error {
	if s.page != nil && !s.page.IsClosed() {
		return nil
	}
	if err := s.launch(cfg.Headless); err != nil {
		return err
	}
	// Always perform a fresh login; do not attempt to reuse an existing session.
	if err := s.login(); err != nil {
		s.close()
		return err
	}
	return nil
}

// login signs in to Leonardo. If the browser already has an authenticated
// session (e.g. in CDP mode), it detects that via the token-balance widget and
// skips the sign-in flow; otherwise it performs the full scripted login.
func (s *browserSession) login() error {
	page := s.page

	// Skip login if already authenticated.
	s.step("checking if already logged in")
	if _, err := page.Goto("https://app.leonardo.ai/", playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err == nil {
		if s.isLoggedIn() {
			log.Println("already logged in, skipping sign-in:", page.URL())
			return nil
		}
	}

	if err := s.gotoLogin(); err != nil {
		return err
	}

	s.step("clicking Continue with Email")
	if err := page.Locator(continueWithEmailSelector).First().Click(
		playwright.LocatorClickOptions{Timeout: playwright.Float(30000)}); err != nil {
		return fmt.Errorf("click continue-with-email: %w", err)
	}

	s.step("filling email")
	if err := page.Locator("#email-field").Fill(cfg.Leonardo.Email,
		playwright.LocatorFillOptions{Timeout: playwright.Float(30000)}); err != nil {
		return fmt.Errorf("fill email: %w", err)
	}

	// The "Continue" button stays disabled until Cloudflare's "verify you are a
	// human" check passes. Wait for it to become enabled (solve the check by hand
	// in the browser window if it doesn't auto-clear).
	s.step("waiting for human-verification check, then enabled Continue button")
	if err := page.Locator(`button:text-is("Continue"):not([disabled])`).First().WaitFor(
		playwright.LocatorWaitForOptions{Timeout: playwright.Float(humanVerifyTimeoutMs)}); err != nil {
		return fmt.Errorf("Continue button never enabled — human-verification not completed: %w", err)
	}

	s.step("clicking Continue")
	if err := page.Locator(`button:text-is("Continue")`).First().Click(
		playwright.LocatorClickOptions{Timeout: playwright.Float(30000)}); err != nil {
		return fmt.Errorf("click Continue: %w", err)
	}

	s.step("filling password")
	if err := page.Locator("#password-field").Fill(cfg.Leonardo.Password,
		playwright.LocatorFillOptions{Timeout: playwright.Float(30000)}); err != nil {
		return fmt.Errorf("fill password: %w", err)
	}

	// The Sign In button may likewise be gated by the human-verification check.
	s.step("waiting for enabled Sign In button")
	if err := page.Locator(`button:text-is("Sign In"):not([disabled])`).First().WaitFor(
		playwright.LocatorWaitForOptions{Timeout: playwright.Float(humanVerifyTimeoutMs)}); err != nil {
		return fmt.Errorf("Sign In button never enabled — human-verification not completed: %w", err)
	}

	s.step("clicking Sign In")
	if err := page.Locator(`button:text-is("Sign In")`).First().Click(
		playwright.LocatorClickOptions{Timeout: playwright.Float(30000)}); err != nil {
		return fmt.Errorf("click Sign In: %w", err)
	}

	s.step("waiting for post-login redirect to app home")
	if err := page.WaitForURL("https://app.leonardo.ai/**", playwright.PageWaitForURLOptions{
		Timeout: playwright.Float(120000),
	}); err != nil {
		return fmt.Errorf("waiting for post-login redirect: %w", err)
	}

	// Confirm we're actually logged in: the token-balance widget only renders for
	// an authenticated session.
	s.step("confirming logged in (token balance widget)")
	if err := s.waitLoggedIn(120000); err != nil {
		return err
	}
	log.Println("login complete:", page.URL())
	return nil
}

// loggedInSelector matches the token-balance widget that is only present when
// signed in. The sidebar renders it twice (collapsed and expanded variants), one
// of them hidden, so match only a visible one.
const loggedInSelector = `[label="View token balance"]:visible, [aria-label="View token balance"]:visible`

// waitLoggedIn waits until the logged-in indicator appears.
func (s *browserSession) waitLoggedIn(timeoutMs float64) error {
	if err := s.page.Locator(loggedInSelector).First().WaitFor(
		playwright.LocatorWaitForOptions{Timeout: playwright.Float(timeoutMs)}); err != nil {
		return fmt.Errorf("not logged in — token balance widget never appeared: %w", err)
	}
	return nil
}

// loggedOutSelector matches what the app shows a signed-out visitor: the
// "Sign In" / "Sign Up" links of the guest view, or the login form itself.
const loggedOutSelector = `a[href="/auth/login"]:visible, ` + continueWithEmailSelector

// isLoggedIn reports whether we are signed in. The app home no longer
// redirects a signed-out visitor to /auth (it shows a guest view instead), so
// the URL says nothing; wait for the SPA to hydrate either the token-balance
// widget (signed in) or a Sign In link / login form (signed out).
func (s *browserSession) isLoggedIn() bool {
	if s.page == nil {
		return false
	}
	either := s.page.Locator(loggedInSelector + `, ` + loggedOutSelector).First()
	if err := either.WaitFor(playwright.LocatorWaitForOptions{Timeout: playwright.Float(15000)}); err != nil {
		if cfg.Debug {
			log.Println("[login-check] neither token balance nor Sign In appeared; assuming signed out")
		}
		return false
	}
	vis, _ := s.page.Locator(loggedInSelector).First().IsVisible()
	return vis
}

// step logs the action about to be attempted (when debug is enabled), waits the
// configured inter-step delay, then attempts to clear any Cloudflare "verify you
// are human" check that may have appeared.
func (s *browserSession) step(desc string) {
	if cfg.Debug {
		url := ""
		if s.page != nil {
			url = s.page.URL()
		}
		log.Printf("[step] %s (at %s)", desc, url)
	}
	if s.page != nil && cfg.StepDelayMs > 0 {
		s.page.WaitForTimeout(float64(cfg.StepDelayMs))
	}
	s.handleHumanVerify()
	s.handleLetsGo()
}

// handleLetsGo dismisses the "Let's Go" button (e.g. an onboarding/welcome
// dialog) if it is present. It is a no-op otherwise, so it is safe to call after
// every step.
func (s *browserSession) handleLetsGo() {
	if s.page == nil {
		return
	}
	btn := s.page.Locator(`button:has-text("Let's Go"), a:has-text("Let's Go")`).First()
	if vis, _ := btn.IsVisible(); !vis {
		return
	}
	if err := btn.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err != nil {
		if cfg.Debug {
			log.Println("[lets-go] found button but could not click it:", err)
		}
		return
	}
	if cfg.Debug {
		log.Println("[lets-go] clicked 'Let's Go' button")
	}
}

// handleHumanVerify looks across all frames for a span reading "Verify you are
// human"; if found, it clicks the checkbox span next to it and then waits ~2s
// with random jitter for the check to settle. It is a no-op when the check isn't
// present, so it is safe to call after every step.
func (s *browserSession) handleHumanVerify() {
	if s.page == nil {
		return
	}
	for _, fr := range s.page.Frames() {
		label := fr.Locator(`xpath=//span[contains(normalize-space(.), "Verify you are human")]`).First()
		if n, _ := label.Count(); n == 0 {
			continue
		}
		if vis, _ := label.IsVisible(); !vis {
			continue
		}
		// Click the element next to the label (the Turnstile checkbox), trying the
		// preceding sibling first, then the following sibling.
		box := fr.Locator(
			`xpath=(//span[contains(normalize-space(.), "Verify you are human")]/preceding-sibling::*[1] | ` +
				`//span[contains(normalize-space(.), "Verify you are human")]/following-sibling::*[1])`).First()
		if err := box.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err != nil {
			if cfg.Debug {
				log.Println("[turnstile] found check but could not click checkbox:", err)
			}
			return
		}
		if cfg.Debug {
			log.Println("[turnstile] clicked 'Verify you are human' checkbox")
		}
		// Wait 2000ms +/- up to 500ms of random jitter.
		jitter := 2000 + rand.Intn(1001) - 500
		s.page.WaitForTimeout(float64(jitter))
		return
	}
}

func (s *browserSession) close() {
	// In CDP mode we attached to the user's browser; disconnect via the browser
	// handle but don't close their context/pages. In launch mode we own the
	// persistent context and close it.
	if s.br != nil {
		s.br.Close() // disconnects from the user's Chrome
		s.br = nil
	} else if s.ctx != nil {
		s.ctx.Close()
	}
	s.ctx = nil
	if s.pw != nil {
		s.pw.Stop()
		s.pw = nil
	}
	s.page = nil
}

// gotoLogin navigates to the Leonardo login page. Sometimes the site won't let
// you go directly to /auth/login and lands you on a landing page instead; in
// that case we click the "Sign In" link (an <a href="/auth/login">) and wait for
// the login page to load.
func (s *browserSession) gotoLogin() error {
	page := s.page
	s.step("navigating directly to login URL")
	if _, err := page.Goto(loginURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
	}); err != nil {
		return fmt.Errorf("goto login: %w", err)
	}
	s.step("checking whether login form is present")

	// If we didn't land on the login form, click the "Sign In" link to reach it.
	emailBtn := page.Locator(continueWithEmailSelector).First()
	if visible, _ := emailBtn.IsVisible(); visible {
		s.step("login form present")
		return nil
	}

	// If we were redirected because we're already logged in, there is no Sign In
	// link — bail out and let the caller detect the logged-in state.
	if s.isLoggedIn() {
		s.step("already logged in; no login form needed")
		return nil
	}

	s.step("login form absent; clicking Sign In link")
	signIn := page.Locator(`a[href="/auth/login"]:visible:has-text("Sign In")`).First()
	if err := signIn.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(15000)}); err != nil {
		return fmt.Errorf("click Sign In link: %w", err)
	}
	if err := emailBtn.WaitFor(playwright.LocatorWaitForOptions{Timeout: playwright.Float(30000)}); err != nil {
		return fmt.Errorf("waiting for login page after Sign In: %w", err)
	}
	s.step("login form loaded after Sign In")
	return nil
}

// interactiveLogin opens a headed browser at the Leonardo login page, best-effort
// pre-fills the credentials from config, then waits for the human to solve the
// Cloudflare Turnstile check and finish signing in. Once the app home loads, the
// session is persisted in the profile directory for headless reuse by the server.
//
// This exists because Cloudflare Turnstile cannot be solved by automation; a
// one-time human sign-in per profile is required.
func (s *browserSession) interactiveLogin() error {
	if err := s.launch(false); err != nil {
		return err
	}
	defer s.close()
	page := s.page

	log.Println("opening login page — please sign in and solve the human-verification check in the browser window...")
	if err := s.gotoLogin(); err != nil {
		return err
	}

	// Best-effort pre-fill to save typing; ignore failures since the user can do
	// it by hand. The Turnstile check and final submit are left to the human.
	s.step("clicking Continue with Email")
	emailBtn := page.Locator(continueWithEmailSelector).First()
	if err := emailBtn.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(10000)}); err == nil {
		s.step("pre-filling email field")
		if ef := page.Locator("#email-field"); ef != nil {
			ef.Fill(cfg.Leonardo.Email, playwright.LocatorFillOptions{Timeout: playwright.Float(10000)})
		}
	} else if cfg.Debug {
		log.Println("[step] Continue with Email click failed (user can proceed manually):", err)
	}

	// Wait (up to 5 minutes) for the human to complete sign-in and land on the app.
	log.Println("waiting for sign-in to complete (up to 5 minutes)...")
	if err := page.WaitForURL("https://app.leonardo.ai/**", playwright.PageWaitForURLOptions{
		Timeout: playwright.Float(300000),
	}); err != nil {
		return fmt.Errorf("timed out waiting for sign-in: %w", err)
	}
	// Small settle so cookies/localStorage flush to the profile before we close.
	page.WaitForTimeout(3000)
	log.Println("login complete, session saved to", cfg.ProfileDir)
	return nil
}

// validSizes is the set of aspect-ratio values accepted by the API; each maps
// to a radio-card value on the generate page.
var validSizes = map[string]bool{"2:3": true, "1:1": true, "16:9": true}

func (s *browserSession) generateLocked(prompt, size string) (string, error) {
	if err := s.ensure(); err != nil {
		return "", err
	}
	url, err := s.generateOnce(prompt, size)
	if err != nil {
		// Session may have expired; restart once and retry.
		log.Println("generation failed, restarting session:", err)
		s.close()
		if err2 := s.ensure(); err2 != nil {
			return "", err2
		}
		url, err = s.generateOnce(prompt, size)
		if err != nil {
			return "", err
		}
	}
	// Log out so the next request starts from a fresh login.
	s.logout()
	return url, nil
}

// profileMenuSelector matches the sidebar avatar button that opens the account
// menu. The page renders two (a collapsed and an expanded sidebar variant), only
// one of them visible, and the generate page renders none.
const profileMenuSelector = `button[aria-label="Profile menu"]:visible`

// logoutItemSelector matches the "Logout" entry in the account menu (the label
// is now a bare text node next to an icon; older builds wrapped it in a span).
const logoutItemSelector = `[role="menuitem"]:text-is("Logout"), [role="menuitem"]:has(span:text-is("Logout"))`

// logout clicks the "Logout" item in the account dropdown menu, ending the
// session. Best effort — failures are logged but not returned, since we already
// have the URL. (The /auth/logout URL does not reliably sign the user out.)
func (s *browserSession) logout() {
	if s.page == nil {
		return
	}
	s.step("logging out via account menu")
	page := s.page
	logoutItem := page.Locator(logoutItemSelector).First()

	// The item lives in a dropdown that may be closed; open the account menu first
	// if the item isn't already visible. The generate page has no sidebar, so go
	// to the app home when the menu button isn't on this page.
	if vis, _ := logoutItem.IsVisible(); !vis {
		trigger := page.Locator(profileMenuSelector).First()
		if vis, _ := trigger.IsVisible(); !vis {
			s.step("profile menu not on this page; going to app home")
			if _, err := page.Goto("https://app.leonardo.ai/", playwright.PageGotoOptions{
				WaitUntil: playwright.WaitUntilStateDomcontentloaded,
				Timeout:   playwright.Float(60000),
			}); err != nil && cfg.Debug {
				log.Println("[logout] could not open app home:", err)
			}
		}
		if err := trigger.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(30000)}); err != nil {
			if cfg.Debug {
				log.Println("[logout] could not open account menu:", err)
			}
		}
	}

	if err := logoutItem.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(15000)}); err != nil {
		log.Println("logout failed:", err)
	} else {
		s.step("clicked Logout")
		// Let the sign-out land before the browser is released.
		if err := page.WaitForURL("**/auth/**", playwright.PageWaitForURLOptions{
			Timeout: playwright.Float(15000),
		}); err != nil && cfg.Debug {
			log.Println("[logout] no redirect to /auth after Logout:", err)
		}
	}
	// Force a fresh browser/login on the next request regardless.
	s.close()
}

func (s *browserSession) generateOnce(prompt, size string) (string, error) {
	page := s.page

	// Navigate to the generate page via the sidebar link if visible, else direct.
	link := page.Locator(`a[href="/generate?model=auto-preset"]`).First()
	if visible, _ := link.IsVisible(); visible {
		if err := link.Click(); err != nil {
			return "", fmt.Errorf("click generate link: %w", err)
		}
	} else {
		if _, err := page.Goto(generateURL, playwright.PageGotoOptions{
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		}); err != nil {
			return "", fmt.Errorf("goto generate: %w", err)
		}
	}
	s.step("navigated to generate page; selecting size " + size)

	// Select the aspect-ratio radio card.
	sizeCard := page.Locator(
		fmt.Sprintf(`[data-slot="radio-card-group-item"][value="%s"]`, size)).First()
	if err := sizeCard.WaitFor(playwright.LocatorWaitForOptions{Timeout: playwright.Float(30000)}); err != nil {
		return "", fmt.Errorf("waiting for size %q radio card: %w", size, err)
	}
	if err := sizeCard.Click(); err != nil {
		return "", fmt.Errorf("select size %q: %w", size, err)
	}
	s.step("selected size; filling prompt")

	ta := page.Locator("#prompt-textarea")
	if err := ta.WaitFor(playwright.LocatorWaitForOptions{Timeout: playwright.Float(30000)}); err != nil {
		return "", fmt.Errorf("waiting for prompt textarea: %w", err)
	}
	if err := ta.Fill(prompt); err != nil {
		return "", fmt.Errorf("fill prompt: %w", err)
	}
	s.step("filled prompt; clicking generate")

	// Remember the generations already listed, so an earlier result at the top of
	// the list is never mistaken for this one.
	items := page.Locator(`div[data-testid^="generation-list-item-"]`)
	before := map[string]bool{}
	if ids, err := items.EvaluateAll(`els => els.map(e => e.dataset.testid)`); err == nil {
		if list, ok := ids.([]any); ok {
			for _, id := range list {
				if tid, ok := id.(string); ok {
					before[tid] = true
				}
			}
		}
	}

	if err := page.Locator(`button[data-tour-id="gen-tour-generate-button"]`).Click(); err != nil {
		return "", fmt.Errorf("click generate: %w", err)
	}
	tasks.Submitted(taskID(prompt))
	s.step("clicked generate; waiting for generation result to appear")

	// The new generation is inserted at the top of the list straight away with a
	// pending-loader image and a temporary test id; the test id switches to the
	// generation id, and the image to the CDN URL, once it finishes. Wait for the
	// first item to be one that wasn't there before, then for its CDN image.
	// Prefer the labelled image (img[data-testid="generation-card-image"]); fall
	// back to any CDN image in the container.
	item := items.First()
	img := item.Locator(
		`img[data-testid="generation-card-image"], img[src^="https://cdn.leonardo.ai/users/"]`).First()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		id, idErr := item.GetAttribute("data-testid", playwright.LocatorGetAttributeOptions{Timeout: playwright.Float(1000)})
		if idErr != nil || before[id] {
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timed out waiting for the new generation to appear in the list")
			}
			page.WaitForTimeout(1000)
			continue
		}
		if src, err := img.GetAttribute("src", playwright.LocatorGetAttributeOptions{Timeout: playwright.Float(1000)}); err == nil &&
			strings.HasPrefix(src, "https://cdn.leonardo.ai/users/") &&
			strings.Contains(src, "/generations/") {
			// Strip the sizing query (e.g. "?w=1250") to return the full-res URL.
			if i := strings.IndexByte(src, '?'); i >= 0 {
				src = src[:i]
			}
			return src, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for generated image src")
		}
		page.WaitForTimeout(1000)
	}
}

// --- HTTP layer ---

const indexHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>Leonardo Image Generator</title>
<style>
  body { font-family: sans-serif; max-width: 700px; margin: 2rem auto; padding: 0 1rem; }
  textarea { width: 100%; height: 6rem; }
  img { max-width: 100%; margin-top: 1rem; }
  #status { color: #666; }
</style>
</head>
<body>
<h1>Leonardo Image Generator</h1>
<textarea id="prompt" placeholder="Enter a prompt..."></textarea><br>
<label>Size:
  <select id="size">
    <option value="16:9">16:9</option>
    <option value="2:3">2:3</option>
    <option value="1:1">1:1</option>
  </select>
</label>
<button id="go">Generate</button>
<p id="status"></p>
<div id="result"></div>
<script>
document.getElementById('go').onclick = async () => {
  const prompt = document.getElementById('prompt').value.trim();
  if (!prompt) return;
  const size = document.getElementById('size').value;
  const status = document.getElementById('status');
  const result = document.getElementById('result');
  status.textContent = 'Generating... (this can take a few minutes)';
  result.innerHTML = '';
  try {
    const resp = await fetch('/api/generate', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({prompt, size})
    });
    const data = await resp.json();
    if (!resp.ok) throw new Error(data.error || resp.statusText);
    status.textContent = 'Done (task ' + data.task_id + ')';
    result.innerHTML = '<a href="' + data.url + '" target="_blank"><img src="' + data.url + '"></a><br><code>' + data.url + '</code>';
  } catch (e) {
    status.textContent = 'Error: ' + e.message;
  }
};
</script>
</body>
</html>`

func handleGenerate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt string `json:"prompt"`
		Size   string `json:"size"`
		// WaitSeconds, if positive, returns 202 with the task id when the image
		// is not ready in time instead of holding the request until it is.
		WaitSeconds int `json:"wait_seconds"`
	}
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
	} else {
		req.Prompt = r.URL.Query().Get("prompt")
		req.Size = r.URL.Query().Get("size")
		if v := r.URL.Query().Get("wait_seconds"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "wait_seconds must be an integer"})
				return
			}
			req.WaitSeconds = n
		}
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt is required"})
		return
	}
	size, err := resolveSize(req.Size)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	log.Printf("generating (size %s): %s", size, req.Prompt)
	id, url, pending, err := generate(req.Prompt, size, time.Duration(max(req.WaitSeconds, 0))*time.Second)
	switch {
	case errors.Is(err, errQueueFull):
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error(), "task_id": id})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error(), "task_id": id})
	case pending:
		info, _ := lookupTask(id)
		writeJSON(w, http.StatusAccepted, info)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"task_id": id, "url": url})
	}
}

// handleTasks returns the task dictionary: task id (prompt hash) ->
// {status, prompt, uri, error, timestamps}, including the task in progress.
func handleTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, tasks.All())
}

// handleTask returns everything known about one task: its id, status (running
// or completed), prompt and image uri.
func handleTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, ok := lookupTask(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown task " + id, "task_id": id})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// resolveSize normalizes an empty/omitted size to the configured default and
// validates it against the allowed aspect ratios.
func resolveSize(size string) (string, error) {
	size = strings.TrimSpace(size)
	if size == "" {
		size = cfg.DefaultSize
	}
	if !validSizes[size] {
		return "", fmt.Errorf("invalid size %q; must be one of 2:3, 1:1, 16:9", size)
	}
	return size, nil
}

func main() {
	loginMode := flag.Bool("login", false, "open a browser to sign in to Leonardo once and save the session to the profile directory")
	tunnelURL := flag.String("tunnel", "", "expose this service through an https-tunnel server at this URL (overrides tunnel.server_url and enables the tunnel)")
	tunnelKey := flag.String("tunnel-key", "", "API key for the tunnel server (default: tunnel.api_key, or the TUNNEL_API_KEY environment variable)")
	tunnelSub := flag.String("tunnel-subdomain", "", "ask the tunnel server for this subdomain label")
	tunnelSession := flag.String("tunnel-session-file", "", "file that persists the tunnel session id, so a restart reclaims the same public URL")
	tunnelOnly := flag.Bool("tunnel-only", false, "serve through the tunnel alone, binding no local port")
	flag.Parse()

	if err := loadConfig(); err != nil {
		log.Fatalf("config: %v", err)
	}
	if *tunnelURL != "" {
		cfg.Tunnel.Enabled = true
		cfg.Tunnel.ServerURL = *tunnelURL
	}
	if *tunnelKey != "" {
		cfg.Tunnel.APIKey = *tunnelKey
	}
	if *tunnelSub != "" {
		cfg.Tunnel.Subdomain = *tunnelSub
	}
	if *tunnelSession != "" {
		cfg.Tunnel.SessionFile = *tunnelSession
	}
	if *tunnelOnly {
		cfg.Tunnel.Enabled = true
		cfg.Tunnel.Only = true
	}
	if err := cfg.Tunnel.validate(); err != nil {
		log.Fatalf("config: %v", err)
	}

	loaded, err := loadTaskStore(cfg.TasksFile)
	if err != nil {
		log.Fatalf("tasks: %v", err)
	}
	tasks = loaded
	queue = newGenerationQueue(*cfg.QueueDepth)
	log.Printf("loaded %d task(s) from %s; queue depth %d", len(tasks.All()), cfg.TasksFile, *cfg.QueueDepth)

	if cfg.ClearDirOnStartup != "" {
		log.Println("clearing directory on startup:", cfg.ClearDirOnStartup)
		if err := os.RemoveAll(cfg.ClearDirOnStartup); err != nil {
			log.Printf("warning: could not clear %q (is Chrome still using it?): %v", cfg.ClearDirOnStartup, err)
		}
	}

	if err := playwright.Install(&playwright.RunOptions{
		Browsers: []string{"chromium"},
	}); err != nil {
		log.Fatalf("installing playwright browsers: %v", err)
	}

	if *loginMode {
		if err := session.interactiveLogin(); err != nil {
			log.Fatalf("login: %v", err)
		}
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})
	mux.HandleFunc("/api/generate", handleGenerate)
	mux.HandleFunc("/api/tasks", handleTasks)
	mux.HandleFunc("GET /api/tasks/{id}", handleTask)
	mux.Handle("/mcp", newMCPTransport(newMCPServer(cfg.MCP.Instructions, cfg.MCP.LegacyCompatibility), cfg.MCP).Handler())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, mux); err != nil {
		log.Fatal(err)
	}
}

// serve runs the local HTTP listener, the tunnel, or both, until ctx is
// cancelled. When both run, either failing takes the other down: a service
// that is half up is worse than one that refuses to start.
func serve(ctx context.Context, handler http.Handler) error {
	if !cfg.Tunnel.Enabled {
		return serveLocal(ctx, handler)
	}
	// The tunnel is public: say so loudly if nothing guards the endpoints.
	if cfg.MCP.AuthToken == "" {
		log.Println("warning: the tunnel is enabled but mcp.auth_token is not set; anyone with the URL can generate images on this account")
	} else {
		log.Println("warning: the tunnel exposes / and /api/generate without authentication; only /mcp checks mcp.auth_token")
	}
	tunnel, err := NewTunnel(&cfg, handler)
	if err != nil {
		return err
	}
	if cfg.Tunnel.Only {
		return tunnel.Run(ctx)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- serveLocal(ctx, handler) }()
	go func() { errs <- tunnel.Run(ctx) }()
	var first error
	for range 2 {
		if err := <-errs; err != nil && first == nil {
			first = err
			cancel()
		}
	}
	return first
}

// serveLocal runs the HTTP server on the configured port until ctx is cancelled.
func serveLocal(ctx context.Context, handler http.Handler) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Println("listening on", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
