package firmware

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is MikroTik's public upgrade server; the RouterOS v7 feed
// lives at <base>/NEWESTa7.<channel>.
const DefaultBaseURL = "https://upgrade.mikrotik.com/routeros"

// Channels are the RouterOS v7 update channels.
var Channels = []string{"stable", "long-term", "testing", "development"}

// ValidChannel reports whether ch is one of Channels.
func ValidChannel(ch string) bool {
	for _, c := range Channels {
		if c == ch {
			return true
		}
	}
	return false
}

// Release is the newest version of one channel.
type Release struct {
	Channel   string
	Version   string
	BuildTime time.Time // zero when the feed did not say
}

// Feed fetches the release feed.
type Feed struct {
	BaseURL string       // DefaultBaseURL when empty
	Client  *http.Client // a 15 s client when nil
}

// maxBody bounds what is read of one feed file; the real one is a line of
// about 20 bytes.
const maxBody = 256

// Fetch reads NEWESTa7.<channel>.
func (f Feed) Fetch(ctx context.Context, channel string) (Release, error) {
	if !ValidChannel(channel) {
		return Release{}, fmt.Errorf("firmware feed: unknown channel %q", channel)
	}
	base := f.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/NEWESTa7."+channel, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("User-Agent", "mikrotik-nms")
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("firmware feed %s: %w", channel, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("firmware feed %s: HTTP %d", channel, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return Release{}, fmt.Errorf("firmware feed %s: %w", channel, err)
	}
	if len(body) > maxBody {
		return Release{}, fmt.Errorf("firmware feed %s: response too large", channel)
	}
	version, built, err := ParseNewest(string(body))
	if err != nil {
		return Release{}, fmt.Errorf("firmware feed %s: %w", channel, err)
	}
	return Release{Channel: channel, Version: version, BuildTime: built}, nil
}

// ParseNewest parses a NEWEST file: "<version> <unix build time>". The build
// time is optional; the version must parse as a RouterOS version.
func ParseNewest(body string) (string, time.Time, error) {
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return "", time.Time{}, errors.New("empty response")
	}
	if len(fields) > 2 {
		return "", time.Time{}, errors.New("unexpected response format")
	}
	if _, ok := ParseVersion(fields[0]); !ok {
		return "", time.Time{}, fmt.Errorf("not a RouterOS version: %.32q", fields[0])
	}
	var built time.Time
	if len(fields) == 2 {
		sec, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || sec <= 0 || sec > 1<<40 {
			return "", time.Time{}, fmt.Errorf("bad build time: %.32q", fields[1])
		}
		built = time.Unix(sec, 0).UTC()
	}
	return fields[0], built, nil
}
