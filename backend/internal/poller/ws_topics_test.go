package poller

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mikrotik-nms/backend/internal/flow"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// The hub refuses a subscription to a topic that is not on its allow-list,
// so a topic published here but missing there would silently never reach a
// browser. Every topic constant and every literal passed to Publish must be
// allowed.
func TestPublishedTopicsAreOnTheWSAllowList(t *testing.T) {
	named := map[string]string{
		"LiveTrafficTopic":   LiveTrafficTopic,
		"PortRatesTopic":     PortRatesTopic,
		"flow.TopicFlushed":  flow.TopicFlushed,
		"flow.TopicStatus":   flow.TopicStatus,
		"trafficTopicPrefix": trafficTopicPrefix + "d1.ether1",
	}
	for name, topic := range named {
		if !ws.TopicAllowed(topic) {
			t.Errorf("%s = %q is not on the WS allow-list (ws/topics.go)", name, topic)
		}
	}

	// Literal topics in the non-test sources of every backend package.
	literal := regexp.MustCompile(`\.Publish\(\s*(?:fmt\.Sprintf\(\s*)?"([^"]+)"`)
	seen := 0
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range literal.FindAllStringSubmatch(string(src), -1) {
			seen++
			topic := strings.ReplaceAll(strings.ReplaceAll(m[1], "%s", "x"), "%d", "1")
			if !ws.TopicAllowed(topic) {
				t.Errorf("%s publishes on %q, which is not on the WS allow-list (ws/topics.go)", path, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 10 {
		t.Fatalf("found only %d literal Publish topics: the scan no longer matches the sources", seen)
	}
}
