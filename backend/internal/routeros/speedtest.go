package routeros

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	ros "github.com/go-routeros/routeros/v3"
)

// SpeedResult summarizes one /tool/fetch download measurement. Mbps is nil
// only transiently inside summarizeFetch error paths — a successful
// FetchSpeedTest always returns it set.
type SpeedResult struct {
	Mbps       *float64
	Bytes      int64
	DurationMs int64
}

// FetchSpeedTest downloads url on the RouterOS device via /tool/fetch
// output=none (payload is discarded device-side) and measures throughput from
// the reported downloaded/duration fields.
//
// MUST be called on a dedicated DialOnce connection with a generous timeout: a
// download can take 60-120s, far past the shared CommandTimeout, and would
// otherwise hold a pooled client's mutex and force-close it under every other
// poller.
func FetchSpeedTest(client *ros.Client, url string, timeout time.Duration) (*SpeedResult, error) {
	start := time.Now()
	reply, err := RunCommandWithTimeout(client, timeout, "/tool/fetch",
		"=url="+url,
		"=output=none",
	)
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(start)
	replies := make([]map[string]string, 0, len(reply.Re))
	for _, re := range reply.Re {
		replies = append(replies, GetSentenceMap(re))
	}
	res, err := summarizeFetch(replies, elapsed)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// summarizeFetch converts raw /tool/fetch reply sentences into a SpeedResult.
// Pure (no I/O) so it is unit-testable against captured sentence maps.
//
// /tool/fetch emits progress !re sentences and a final one with status
// "finished"; fields seen across RouterOS versions: "downloaded" (KiB, may
// carry a "KiB" suffix), "total" (KiB), "duration" (RouterOS duration like
// "8s120ms" or plain "12s"). The LAST sentence with status=="finished" is
// preferred (else the last sentence, which then fails the finished check).
// bytes = downloaded KiB × 1024; Mbps = bytes×8 / seconds / 1e6.
//
// Real devices report duration in WHOLE seconds (live capture: '1s'), so a
// sub-second download reports 0 and a 1-4s one quantizes badly. When the
// reported duration is below 5s, the caller-measured wall-clock elapsed is
// used instead — better resolution; it includes connect overhead, which is
// acceptable. An error is returned when the fetch never finished, finished
// with zero bytes, or has no usable duration (reported and elapsed both ~0).
func summarizeFetch(replies []map[string]string, elapsed time.Duration) (SpeedResult, error) {
	if len(replies) == 0 {
		return SpeedResult{}, fmt.Errorf("fetch returned no progress sentences")
	}

	final := replies[len(replies)-1]
	for i := len(replies) - 1; i >= 0; i-- {
		if replies[i]["status"] == "finished" {
			final = replies[i]
			break
		}
	}

	res := SpeedResult{}
	if kib, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(final["downloaded"]), "KiB")), 64); err == nil {
		res.Bytes = int64(kib * 1024)
	}
	if ms, ok := parseROSDuration(final["duration"]); ok {
		res.DurationMs = int64(ms)
	}
	if res.DurationMs < 5000 && elapsed > 0 {
		res.DurationMs = elapsed.Milliseconds()
	}

	if status := final["status"]; status != "finished" {
		if status == "" {
			status = "unknown"
		}
		return res, fmt.Errorf("fetch did not finish (status %q)", status)
	}
	if res.DurationMs == 0 {
		return res, fmt.Errorf("fetch finished with zero duration — cannot derive throughput")
	}
	if res.Bytes == 0 {
		return res, fmt.Errorf("fetch finished with zero bytes downloaded")
	}

	mbps := float64(res.Bytes) * 8 / (float64(res.DurationMs) / 1000) / 1e6
	res.Mbps = &mbps
	return res, nil
}
