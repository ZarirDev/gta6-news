// gta6recomp-monitor.go
// Checks the GTA6 Recomp blog for newly added .post-row entries and notifies Discord.
// Uses only the Go standard library. In GitHub Actions, set RUN_ONCE=1 so one
// workflow execution performs one check and exits. Locally, it polls continuously.
//
// Required environment variable: DISCORD_WEBHOOK_URL (keep this secret; never put it in JSON)
// Optional: WEBHOOK_NAME, CHECK_INTERVAL, RUN_ONCE, STATE_FILE

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const defaultBlogURL = "https://gta6recomp.com/blog"
const stateVersion = 1

type Post struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Date        string `json:"date,omitempty"`
	Category    string `json:"category,omitempty"`
	Description string `json:"description,omitempty"`
}

type MonitorConfig struct {
	SiteURL       string `json:"site_url"`
	Selector      string `json:"post_selector"`
	CheckInterval string `json:"check_interval"`
	WebhookName   string `json:"webhook_name"`
	StartedAt     string `json:"created_at"`
}

type State struct {
	Version       int             `json:"version"`
	Config        MonitorConfig   `json:"config"`
	Initialized   bool            `json:"initialized"`
	SeenPosts     map[string]Post `json:"seen_posts"`
	TotalNotified int             `json:"total_notified"`
	LastNewPostAt string          `json:"last_new_post_at,omitempty"`
}

type DiscordPayload struct {
	Embeds          []DiscordEmbed `json:"embeds"`
	AllowedMentions struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
}

type DiscordEmbed struct {
	Title       string       `json:"title"`
	URL         string       `json:"url"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color"`
	Fields      []EmbedField `json:"fields,omitempty"`
	Footer      *EmbedFooter `json:"footer,omitempty"`
}

type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type EmbedFooter struct {
	Text string `json:"text"`
}

var (
	anchorStartRE = regexp.MustCompile(`(?i)<a\b[^>]*>`)
	classRE       = regexp.MustCompile(`(?i)\bclass\s*=\s*["']([^"']*)["']`)
	hrefRE        = regexp.MustCompile(`(?i)\bhref\s*=\s*["']([^"']+)["']`)
	timeRE        = regexp.MustCompile(`(?i)<time\b[^>]*\bdatetime\s*=\s*["']([^"']+)["']`)
	titleRE       = regexp.MustCompile(`(?is)<h3\b[^>]*>(.*?)</h3>`)
	descRE        = regexp.MustCompile(`(?is)<p\b[^>]*>(.*?)</p>`)
	tagRE         = regexp.MustCompile(`(?is)<span\b[^>]*class\s*=\s*["'][^"']*\btag\b[^"']*["'][^>]*>(.*?)</span>`)
	stripTagsRE   = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRE       = regexp.MustCompile(`\s+`)
)

func main() {
	webhook := strings.TrimSpace(os.Getenv("DISCORD_WEBHOOK_URL"))
	if webhook == "" {
		log.Fatal("DISCORD_WEBHOOK_URL is required (set it as a GitHub Actions secret)")
	}
	if !strings.HasPrefix(webhook, "https://discord.com/api/webhooks/") &&
		!strings.HasPrefix(webhook, "https://discordapp.com/api/webhooks/") {
		log.Fatal("DISCORD_WEBHOOK_URL does not look like a Discord webhook URL")
	}

	interval := 5 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("CHECK_INTERVAL")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < time.Minute {
			log.Fatal("CHECK_INTERVAL must be at least 1m (for example 5m)")
		}
		interval = parsed
	}

	statePath := strings.TrimSpace(os.Getenv("STATE_FILE"))
	if statePath == "" {
		statePath = "monitor.json"
	}
	webhookName := strings.TrimSpace(os.Getenv("WEBHOOK_NAME"))
	if webhookName == "" {
		webhookName = "GTA6 Recomp Blog Alerts"
	}

	state, err := loadState(statePath)
	if err != nil {
		log.Fatalf("load %s: %v", statePath, err)
	}
	if err := applyConfig(&state, interval, webhookName); err != nil {
		log.Fatalf("prepare monitor configuration: %v", err)
	}
	// Persist safe startup/configuration metadata. This is idempotent, so the
	// GitHub Actions workflow only creates a commit when the JSON really changes.
	if err := saveState(statePath, state); err != nil {
		log.Fatalf("save monitor configuration: %v", err)
	}

	client := &http.Client{Timeout: 25 * time.Second}
	log.Printf("Monitor: %s | selector: %s | interval: %s | Discord destination label: %s",
		state.Config.SiteURL, state.Config.Selector, state.Config.CheckInterval, state.Config.WebhookName)
	log.Printf("State file: %s | known posts: %d | initialized: %t", statePath, len(state.SeenPosts), state.Initialized)

	if err := checkOnce(client, webhook, statePath, &state); err != nil {
		log.Fatalf("check failed: %v", err)
	}

	if strings.EqualFold(os.Getenv("RUN_ONCE"), "1") || strings.EqualFold(os.Getenv("RUN_ONCE"), "true") {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("Continuous mode enabled; next check in %s", interval)
	for range ticker.C {
		if err := checkOnce(client, webhook, statePath, &state); err != nil {
			log.Printf("Check failed (will retry next interval): %v", err)
		}
	}
}

func applyConfig(state *State, interval time.Duration, webhookName string) error {
	changed := false
	if state.Version == 0 {
		state.Version = stateVersion
		changed = true
	}
	if state.SeenPosts == nil {
		state.SeenPosts = make(map[string]Post)
		changed = true
	}
	if state.Config.SiteURL != defaultBlogURL {
		state.Config.SiteURL = defaultBlogURL
		changed = true
	}
	if state.Config.Selector != "a.post-row" {
		state.Config.Selector = "a.post-row"
		changed = true
	}
	if state.Config.CheckInterval != interval.String() {
		state.Config.CheckInterval = interval.String()
		changed = true
	}
	if state.Config.WebhookName != webhookName {
		state.Config.WebhookName = webhookName
		changed = true
	}
	if state.Config.StartedAt == "" {
		state.Config.StartedAt = time.Now().UTC().Format(time.RFC3339)
		changed = true
	}
	if changed {
		return nil // caller persists it together with the first check/baseline
	}
	return nil
}

func checkOnce(client *http.Client, webhook, statePath string, state *State) error {
	posts, err := fetchPosts(client, state.Config.SiteURL)
	if err != nil {
		return err
	}

	// First successful fetch is the baseline. Do not send a burst of alerts for
	// posts that were already present before the monitor was installed.
	if !state.Initialized {
		for _, post := range posts {
			state.SeenPosts[post.URL] = post
		}
		state.Initialized = true
		if err := saveState(statePath, *state); err != nil {
			return fmt.Errorf("save initial baseline: %w", err)
		}
		log.Printf("Initial baseline saved: %d existing posts; no notifications sent.", len(posts))
		return nil
	}

	newPosts := make([]Post, 0)
	for _, post := range posts {
		if _, exists := state.SeenPosts[post.URL]; !exists {
			newPosts = append(newPosts, post)
		}
	}
	if len(newPosts) == 0 {
		log.Printf("No new posts. Tracking %d posts.", len(state.SeenPosts))
		return nil
	}

	// Process oldest-to-newest among newly discovered posts, so a backlog is
	// announced in chronological order when several posts appear between runs.
	for i := len(newPosts) - 1; i >= 0; i-- {
		post := newPosts[i]
		log.Printf("New post detected: %s", post.Title)
		if err := sendDiscord(client, webhook, state.Config.WebhookName, post); err != nil {
			return fmt.Errorf("Discord notification for %s: %w", post.URL, err)
		}
		state.SeenPosts[post.URL] = post
		state.TotalNotified++
		state.LastNewPostAt = time.Now().UTC().Format(time.RFC3339)
		// Persist after every successful send to reduce duplicate alerts if a
		// later notification in this same run fails.
		if err := saveState(statePath, *state); err != nil {
			return fmt.Errorf("save state after notifying %s: %w", post.URL, err)
		}
	}
	log.Printf("Sent %d new-post notification(s).", len(newPosts))
	return nil
}

func fetchPosts(client *http.Client, url string) ([]Post, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "GTA6RecompBlogMonitor/1.1 (personal blog update monitor)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("website returned HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}
	posts := parsePosts(string(body))
	if len(posts) == 0 {
		return nil, errors.New("found no a.post-row entries; refusing to treat an empty/changed page as valid")
	}
	return posts, nil
}

func parsePosts(document string) []Post {
	var posts []Post
	lower := strings.ToLower(document)
	for _, loc := range anchorStartRE.FindAllStringIndex(document, -1) {
		openTag := document[loc[0]:loc[1]]
		classMatch := classRE.FindStringSubmatch(openTag)
		if len(classMatch) < 2 || !hasClass(classMatch[1], "post-row") {
			continue
		}
		closeOffset := strings.Index(lower[loc[1]:], "</a>")
		if closeOffset < 0 {
			continue
		}
		inner := document[loc[1] : loc[1]+closeOffset]
		hrefMatch := hrefRE.FindStringSubmatch(openTag)
		titleMatch := titleRE.FindStringSubmatch(inner)
		if len(hrefMatch) < 2 || len(titleMatch) < 2 {
			continue
		}
		href := strings.TrimSpace(html.UnescapeString(hrefMatch[1]))
		if strings.HasPrefix(href, "/") {
			href = "https://gta6recomp.com" + href
		}
		if !strings.HasPrefix(href, "https://gta6recomp.com/") {
			continue
		}
		post := Post{URL: href, Title: cleanText(titleMatch[1])}
		if m := timeRE.FindStringSubmatch(inner); len(m) > 1 {
			post.Date = html.UnescapeString(strings.TrimSpace(m[1]))
		}
		if m := tagRE.FindStringSubmatch(inner); len(m) > 1 {
			post.Category = cleanText(m[1])
		}
		if m := descRE.FindStringSubmatch(inner); len(m) > 1 {
			post.Description = cleanText(m[1])
		}
		if post.Title != "" {
			posts = append(posts, post)
		}
	}
	return posts
}

func hasClass(classList, wanted string) bool {
	for _, className := range strings.Fields(classList) {
		if className == wanted {
			return true
		}
	}
	return false
}

func cleanText(value string) string {
	value = stripTagsRE.ReplaceAllString(value, " ")
	value = html.UnescapeString(value)
	return strings.TrimSpace(spaceRE.ReplaceAllString(value, " "))
}

func sendDiscord(client *http.Client, webhook, webhookName string, post Post) error {
	description := post.Description
	if len(description) > 3500 {
		description = description[:3500] + "…"
	}
	embed := DiscordEmbed{
		Title: post.Title, URL: post.URL, Description: description,
		Color: 0x8B5CF6, Footer: &EmbedFooter{Text: webhookName + " • New blog post"},
	}
	if post.Category != "" || post.Date != "" {
		embed.Fields = append(embed.Fields,
			EmbedField{Name: "Category", Value: fallback(post.Category, "Uncategorized"), Inline: true},
			EmbedField{Name: "Published", Value: fallback(post.Date, "Unknown"), Inline: true},
		)
	}
	payload := DiscordPayload{Embeds: []DiscordEmbed{embed}}
	payload.AllowedMentions.Parse = []string{}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, webhook, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("Discord returned HTTP %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func fallback(value, defaultValue string) string {
	if strings.TrimSpace(value) == "" {
		return defaultValue
	}
	return value
}

func loadState(path string) (State, error) {
	state := State{Version: stateVersion, SeenPosts: make(map[string]Post)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("invalid JSON: %w", err)
	}
	if state.Version != 0 && state.Version != stateVersion {
		return state, fmt.Errorf("unsupported state file version %d", state.Version)
	}
	if state.SeenPosts == nil {
		state.SeenPosts = make(map[string]Post)
	}
	return state, nil
}

func saveState(path string, state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
