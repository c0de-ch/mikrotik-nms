package poller

import (
	"context"
	"database/sql"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// defaultSpeedTestInterval is the cycle period when the admin hasn't
// configured speedtest_interval. Speed tests are heavyweight (real downloads
// that saturate a WAN for many seconds), so the default is deliberately long.
const defaultSpeedTestInterval = 6 * time.Hour

// speedTestStartupDelay holds the first scheduled cycle back after backend
// start so every deploy/restart doesn't trigger a download burst; run-now
// covers immediate needs.
const speedTestStartupDelay = 10 * time.Minute

// speedTestFetchTimeout bounds one /tool/fetch download on its dedicated
// connection.
const speedTestFetchTimeout = 120 * time.Second

// SpeedTestPoller runs the configured speed_tests on a schedule: a /tool/fetch
// download executed ON the RouterOS device, measured from the device's own
// downloaded/duration progress fields, persisted as speed_samples and
// broadcast on the "connectivity.speed" topic.
//
// Tests run SEQUENTIALLY (one device at a time) so parallel downloads don't
// skew each other's results, and each uses a one-shot dedicated connection
// (routeros.DialOnce): a download can take 60-120s, which would hold a pooled
// client's mutex past CommandTimeout and force-close it under every other
// poller.
type SpeedTestPoller struct {
	db        *sql.DB
	hub       *ws.Hub
	verifyTLS bool
	interval  time.Duration
}

func NewSpeedTestPoller(db *sql.DB, hub *ws.Hub, verifyTLS bool, interval time.Duration) *SpeedTestPoller {
	return &SpeedTestPoller{db: db, hub: hub, verifyTLS: verifyTLS, interval: interval}
}

// currentInterval reads the runtime-tunable speedtest_interval (seconds) from
// app_settings, falling back to the constructed interval. Re-read each cycle
// so a Settings change applies without a backend restart. Parsed values are
// clamped to 300..604800 (5 minutes to a week): anything more frequent would
// keep the WAN saturated, anything longer (or a huge pasted number) would
// overflow into a useless or negative timer.
func (sp *SpeedTestPoller) currentInterval() time.Duration {
	if v, err := queries.GetSetting(sp.db, "speedtest_interval"); err == nil {
		if n, e := strconv.Atoi(strings.TrimSpace(v)); e == nil {
			if n < 300 {
				n = 300
			}
			if n > 604800 {
				n = 604800
			}
			return time.Duration(n) * time.Second
		}
	}
	return sp.interval
}

func (sp *SpeedTestPoller) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(speedTestStartupDelay):
	}
	sp.safePoll(ctx)

	for {
		timer := time.NewTimer(sp.currentInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			sp.safePoll(ctx)
		}
	}
}

func (sp *SpeedTestPoller) safePoll(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("speedtest: panic: %v", r)
		}
	}()
	sp.poll(ctx)
}

func (sp *SpeedTestPoller) poll(ctx context.Context) {
	tests, err := queries.ListEnabledSpeedTests(sp.db)
	if err != nil {
		log.Printf("speedtest: list tests: %v", err)
		return
	}
	if len(tests) == 0 {
		return // nothing configured — keep the idle cycle free of any device traffic
	}

	for i, test := range tests {
		select {
		case <-ctx.Done():
			return
		default:
		}
		sp.safeRunTest(test)
		if i < len(tests)-1 {
			time.Sleep(2 * time.Second) // let the line settle between tests
		}
	}
}

// safeRunTest runs one test with a panic guard so a single bad test can't
// abort the rest of the cycle. The shared run guard keeps a scheduled run from
// overlapping an API run-now download for the same test.
func (sp *SpeedTestPoller) safeRunTest(test queries.SpeedTest) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("speedtest: panic running test %s: %v", test.ID, r)
		}
	}()
	if !TryBeginRun("speedtest:" + test.ID) {
		log.Printf("speedtest: %s already running, skipping this cycle", test.ID)
		return
	}
	defer EndRun("speedtest:" + test.ID)
	sample := RunSpeedTest(sp.db, test, sp.verifyTLS)
	sp.persistAndPublish(sample)
}

// RunSpeedTest executes one speed test synchronously on a dedicated connection
// and returns the resulting (unpersisted) sample. Failures — missing/offline
// device, dial error, fetch trap/timeout/unfinished — come back as error
// samples (mbps nil, error set) so the time series shows gaps honestly. Shared
// by the scheduled poller and the API's run-now endpoint so both measure
// identically.
func RunSpeedTest(db *sql.DB, test queries.SpeedTest, verifyTLS bool) *queries.SpeedSample {
	sample := &queries.SpeedSample{TestID: test.ID, DeviceID: test.DeviceID}

	dev, err := queries.GetDevice(db, test.DeviceID)
	if err != nil {
		sample.Error = "device no longer exists"
		return sample
	}
	if dev.Status != "online" {
		sample.Error = "device is " + dev.Status
		return sample
	}

	client, err := routeros.DialOnce(dev.Address, dev.APIPort, dev.Username, dev.PasswordEnc, dev.UseTLS, verifyTLS)
	if err != nil {
		sample.Error = err.Error()
		return sample
	}
	defer client.Close()

	res, err := routeros.FetchSpeedTest(client, test.URL, test.SrcAddress, speedTestFetchTimeout)
	if err != nil {
		sample.Error = err.Error()
		return sample
	}
	sample.Mbps = res.Mbps
	sample.Bytes = res.Bytes
	sample.DurationMs = res.DurationMs
	return sample
}

// persistAndPublish writes the sample to SQLite FIRST (the WS publish drops
// for slow clients, so the DB is the source of truth), then broadcasts it.
func (sp *SpeedTestPoller) persistAndPublish(s *queries.SpeedSample) {
	if err := queries.InsertSpeedSample(sp.db, s); err != nil {
		log.Printf("speedtest: insert sample for test %s: %v", s.TestID, err)
		return
	}
	sp.hub.Publish("connectivity.speed", s)
}
