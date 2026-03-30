package resolver

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// Resolver performs reverse DNS lookups against configured DNS servers.
type Resolver struct {
	db    *sql.DB
	cache sync.Map // ip -> cachedEntry
}

type cachedEntry struct {
	name    string
	expires time.Time
}

const cacheTTL = 5 * time.Minute

func New(db *sql.DB) *Resolver {
	return &Resolver{db: db}
}

// ResolveIP performs a reverse DNS lookup for the given IP address
// against all enabled DNS servers. Returns the first successful result.
func (r *Resolver) ResolveIP(ip string) string {
	if ip == "" {
		return ""
	}

	// Check cache
	if entry, ok := r.cache.Load(ip); ok {
		ce := entry.(cachedEntry)
		if time.Now().Before(ce.expires) {
			return ce.name
		}
		r.cache.Delete(ip)
	}

	servers, err := queries.ListEnabledDNSServers(r.db)
	if err != nil || len(servers) == 0 {
		// Fallback to system resolver
		name := systemReverseLookup(ip)
		r.cache.Store(ip, cachedEntry{name: name, expires: time.Now().Add(cacheTTL)})
		return name
	}

	for _, srv := range servers {
		name := reverseLookup(ip, fmt.Sprintf("%s:%d", srv.Address, srv.Port))
		if name != "" {
			r.cache.Store(ip, cachedEntry{name: name, expires: time.Now().Add(cacheTTL)})
			return name
		}
	}

	// Cache the miss too
	r.cache.Store(ip, cachedEntry{name: "", expires: time.Now().Add(cacheTTL)})
	return ""
}

// ResolveMany resolves multiple IPs concurrently.
func (r *Resolver) ResolveMany(ips []string) map[string]string {
	results := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Limit concurrency
	sem := make(chan struct{}, 20)

	for _, ip := range ips {
		if ip == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(addr string) {
			defer wg.Done()
			defer func() { <-sem }()
			name := r.ResolveIP(addr)
			if name != "" {
				mu.Lock()
				results[addr] = name
				mu.Unlock()
			}
		}(ip)
	}

	wg.Wait()
	return results
}

func reverseLookup(ip, server string) string {
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 2 * time.Second}
			return d.DialContext(ctx, "udp", server)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	names, err := resolver.LookupAddr(ctx, ip)
	if err != nil || len(names) == 0 {
		return ""
	}

	// Remove trailing dot
	name := strings.TrimSuffix(names[0], ".")
	return name
}

func systemReverseLookup(ip string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	names, err := net.DefaultResolver.LookupAddr(ctx, ip)
	if err != nil || len(names) == 0 {
		return ""
	}
	return strings.TrimSuffix(names[0], ".")
}
