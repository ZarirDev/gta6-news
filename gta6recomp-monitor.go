package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"
)

const stateFile = "monitor.json"

// Config is persisted in monitor.json. Keep webhook credentials in an environment
// variable, never in this file.
type Config struct {
	SiteURL       string `json:"site_url"`
	PostsJSONURL  string `json:"posts_json_url"`
	CheckInterval string `json:"check_interval"`
	WebhookName   string `json:"webhook_name"`
}

type Post struct {
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	Date    string `json:"date"`
	Tag     string `json:"tag"`
	Author  string `json:"author"`
	Repo    string `json:"repo"`
	Image   string `json:"image"`
	Excerpt string `json:"excerpt"`
	Draft   bool   `json:"draft"`
}

type State struct {
	Version       int             `json:"version"`
	Config        Config          `json:"config"`
	Initialized   bool            `json:"initialized"`
	SeenPosts     map[string]Post `json:"seen_posts"`
	TotalNotified int             `json:"total_notified"`
	LastNewPostAt string          `json:"last_new_post_at,omitempty"`
}

type DiscordPayload struct {
	Username string         `json:"username,omitempty"`
	Embeds   []DiscordEmbed `json:"embeds"`
}
type DiscordEmbed struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	Color       int    `json:"color"`
	Fields      []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Inline bool   `json:"inline,omitempty"`
	} `json:"fields,omitempty"`
	Footer struct {
		Text string `json:"text"`
	} `json:"footer,omitempty"`
}

func defaultConfig() Config {
	return Config{
		SiteURL:       "https://gta6recomp.com/blog",
		PostsJSONURL:  "https://gta6recomp.com/data/posts.json",
		CheckInterval: "5m",
		WebhookName:   "GTA 6 News",
	}
}

func loadState() (State, error) {
	b, err := os.ReadFile(stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return State{Version: 1, Config: defaultConfig(), SeenPosts: map[string]Post{}}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("read %s: %w", stateFile, err)
	}
	if s.Version == 0 {
		s.Version = 1
	}
	if s.Config.SiteURL == "" {
		s.Config.SiteURL = defaultConfig().SiteURL
	}
	if s.Config.PostsJSONURL == "" {
		s.Config.PostsJSONURL = defaultConfig().PostsJSONURL
	}
	if s.Config.CheckInterval == "" {
		s.Config.CheckInterval = defaultConfig().CheckInterval
	}
	if s.Config.WebhookName == "" {
		s.Config.WebhookName = defaultConfig().WebhookName
	}
	if s.SeenPosts == nil {
		s.SeenPosts = map[string]Post{}
	}
	return s, nil
}

func saveState(s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := stateFile + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, stateFile)
}

func fetchPosts(endpoint string) ([]Post, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "GTA6RecompMonitor/1.0 (+blog update notifier)")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch posts JSON: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch posts JSON: HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}
	var posts []Post
	if err := json.Unmarshal(body, &posts); err != nil {
		return nil, fmt.Errorf("decode posts JSON: %w", err)
	}
	if len(posts) == 0 {
		return nil, errors.New("posts JSON contained zero entries; refusing to treat it as valid")
	}
	return posts, nil
}

func postURL(siteURL, slug string) string {
	u, err := url.Parse(siteURL)
	if err != nil {
		return "https://gta6recomp.com/post/" + url.PathEscape(slug)
	}
	u.Path = path.Join(path.Dir(u.Path), "post", slug)
	return u.String()
}

func sendDiscord(webhook, username, siteURL string, p Post) error {
	return sendDiscordPost(webhook, username, siteURL, p, "GTA6 Recomp Blog Monitor")
}

func sendDiscordPost(webhook, username, siteURL string, p Post, footer string) error {
	if webhook == "" {
		return errors.New("DISCORD_WEBHOOK_URL is not set")
	}
	if p.Title == "" || p.Slug == "" {
		return errors.New("post is missing title or slug")
	}
	if len(p.Excerpt) > 3500 {
		p.Excerpt = p.Excerpt[:3497] + "..."
	}
	embed := DiscordEmbed{Title: p.Title, URL: postURL(siteURL, p.Slug), Description: p.Excerpt, Color: 5793266}
	if p.Date != "" || p.Tag != "" {
		f := struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Inline bool   `json:"inline,omitempty"`
		}{Name: "Published", Value: strings.TrimSpace(p.Date + "  •  " + strings.ToUpper(p.Tag)), Inline: true}
		embed.Fields = append(embed.Fields, f)
	}
	if p.Repo != "" {
		f := struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Inline bool   `json:"inline,omitempty"`
		}{Name: "Project", Value: p.Repo, Inline: true}
		embed.Fields = append(embed.Fields, f)
	}
	embed.Footer.Text = footer
	payload := DiscordPayload{Username: username, Embeds: []DiscordEmbed{embed}}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Discord notification: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, webhook, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send Discord notification: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("Discord webhook returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func sendDiscordTest(webhook, username, siteURL string, state State) error {
	if webhook == "" {
		return errors.New("TEST_DISCORD=1 requires DISCORD_WEBHOOK_URL to be set")
	}
	if len(state.SeenPosts) == 0 {
		return errors.New("monitor.json has no recorded posts to test with; run the monitor once first")
	}

	var latest Post
	found := false
	for _, post := range state.SeenPosts {
		if post.Slug == "" || post.Title == "" {
			continue
		}
		if !found || post.Date > latest.Date || (post.Date == latest.Date && post.Slug > latest.Slug) {
			latest = post
			found = true
		}
	}
	if !found {
		return errors.New("monitor.json contains no valid recorded posts with a title and slug")
	}
	if err := sendDiscordPost(webhook, username, siteURL, latest, "TEST • Last recorded post from monitor.json"); err != nil {
		return err
	}
	log.Printf("Sent test notification using last recorded post: %s (%s)", latest.Title, latest.Slug)
	return nil
}

func runCheck(s *State, webhook string) error {
	posts, err := fetchPosts(s.Config.PostsJSONURL)
	if err != nil {
		return err
	}
	current := make(map[string]Post, len(posts))
	for _, p := range posts {
		if p.Draft || strings.TrimSpace(p.Slug) == "" {
			continue
		}
		current[p.Slug] = p
	}
	if len(current) == 0 {
		return errors.New("no published posts found in JSON; refusing to update state")
	}

	if !s.Initialized {
		s.SeenPosts = current
		s.Initialized = true
		if err := saveState(*s); err != nil {
			return err
		}
		log.Printf("Initial baseline saved: %d published posts; no notifications sent.", len(current))
		return nil
	}

	newPosts := make([]Post, 0)
	for slug, p := range current {
		if _, exists := s.SeenPosts[slug]; !exists {
			newPosts = append(newPosts, p)
		}
	}
	for i := 0; i < len(newPosts); i++ {
		for j := i + 1; j < len(newPosts); j++ {
			if newPosts[j].Date < newPosts[i].Date {
				newPosts[i], newPosts[j] = newPosts[j], newPosts[i]
			}
		}
	}
	for _, p := range newPosts {
		if err := sendDiscord(webhook, s.Config.WebhookName, s.Config.SiteURL, p); err != nil {
			return err
		}
		s.SeenPosts[p.Slug] = p
		s.TotalNotified++
		s.LastNewPostAt = time.Now().UTC().Format(time.RFC3339)
		if err := saveState(*s); err != nil {
			return fmt.Errorf("notification sent, but saving state failed: %w", err)
		}
		log.Printf("Notified Discord about new post: %s", p.Title)
	}
	for slug, p := range current {
		if _, ok := s.SeenPosts[slug]; ok {
			s.SeenPosts[slug] = p
		}
	}
	if err := saveState(*s); err != nil {
		return err
	}
	if len(newPosts) == 0 {
		log.Printf("No new posts. Checked %d published posts.", len(current))
	}
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags)
	s, err := loadState()
	if err != nil {
		log.Fatal(err)
	}
	if err := saveState(s); err != nil {
		log.Fatal(err)
	}
	interval, err := time.ParseDuration(s.Config.CheckInterval)
	if err != nil || interval < time.Minute {
		log.Fatalf("invalid check_interval %q (must be at least 1m)", s.Config.CheckInterval)
	}
	webhook := strings.TrimSpace(os.Getenv("DISCORD_WEBHOOK_URL"))

	// Keep the one-shot test mode, but never let it enter the polling loop.
	if os.Getenv("TEST_DISCORD") == "1" {
		if err := sendDiscordTest(webhook, s.Config.WebhookName, s.Config.SiteURL, s); err != nil {
			log.Fatal(err)
		}
		log.Println("Discord test message sent successfully. monitor.json and post history were not changed.")
		return
	}

	runOnce := os.Getenv("RUN_ONCE") == "1"
	log.Printf("Monitor: %s | JSON feed: %s | interval: %s | Discord destination label: %s", s.Config.SiteURL, s.Config.PostsJSONURL, interval, s.Config.WebhookName)
	log.Printf("State file: %s | known posts: %d | initialized: %t", stateFile, len(s.SeenPosts), s.Initialized)

	if runOnce {
		if err := runCheck(&s, webhook); err != nil {
			log.Printf("check failed: %v", err)
			os.Exit(1)
		}
		return
	}

	// SIGINT (Ctrl+C) and SIGTERM (service stop) request a clean shutdown.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	log.Printf("Continuous monitoring enabled. Next checks will run every %s. Press Ctrl+C to stop.", interval)
	if err := runCheck(&s, webhook); err != nil {
		log.Printf("check failed (will retry after %s): %v", interval, err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			log.Println("Shutdown signal received. Saving state and exiting.")
			if err := saveState(s); err != nil {
				log.Printf("final state save failed: %v", err)
				os.Exit(1)
			}
			log.Println("Monitor stopped cleanly.")
			return
		case <-ticker.C:
			if err := runCheck(&s, webhook); err != nil {
				log.Printf("check failed (will retry at next interval): %v", err)
			}
		}
	}
}
