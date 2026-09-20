// Package cache wraps the RESP client (ADR-001 Q-004).
//
// The managed container is Valkey; EXTERNAL is any RESP-compatible server
// >= 7.2. Vizra uses only the Redis 7.2 / Valkey 7.2 command set and no
// modules, which is why the same client and the same code answer both, and why
// core's integration lane runs a two-image matrix for the life of the project:
// go-redis publishes no Valkey support statement and Valkey publishes no formal
// protocol-compatibility guarantee, so compatibility is asserted by a test, not
// by a README.
//
// Eviction is safe here — the compose file sets maxmemory and allkeys-lru —
// because nothing durable lives in the cache: durable work is in PostgreSQL
// (ADR-004). The golden path must pass after FLUSHALL, and that is a test.
package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Flavour is what `vizra doctor` prints and what the integration matrix
// distinguishes.
type Flavour string

const (
	FlavourValkey  Flavour = "valkey"
	FlavourRedis   Flavour = "redis"
	FlavourUnknown Flavour = "unknown"
)

// ServerInfo is the identity of the connected server.
type ServerInfo struct {
	Flavour Flavour
	Version string
}

func (s ServerInfo) String() string {
	if s.Version == "" {
		return string(s.Flavour)
	}
	return string(s.Flavour) + " " + s.Version
}

// Client is the cache handle. It is resolved from the site, never held in a
// package global (Q-008 checklist item 1).
type Client struct {
	rdb       *redis.Client
	namespace string
}

// Open parses the URL and builds a client. It does not connect: connection
// happens on first use, so a cache that is down at boot degrades readiness
// rather than preventing boot.
func Open(url, namespace string) (*Client, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		// Never echo the URL: it can carry a password.
		return nil, errors.New("cache: VIZRA_CACHE_URL is not a valid redis:// or rediss:// URL")
	}
	opt.MaxRetries = 2
	opt.DialTimeout = 2 * time.Second
	opt.ReadTimeout = 2 * time.Second
	opt.WriteTimeout = 2 * time.Second
	return &Client{rdb: redis.NewClient(opt), namespace: namespace}, nil
}

// Close releases the connection pool.
func (c *Client) Close() error { return c.rdb.Close() }

// Redis exposes the underlying client for packages that need commands this
// wrapper does not forward. It is deliberately explicit rather than embedded,
// so a grep finds every caller that reaches past the wrapper.
func (c *Client) Redis() *redis.Client { return c.rdb }

// Ping is the readiness check. A failure is DEGRADED, not fatal: the API falls
// back to a per-process in-memory rate limiter (ADR-003).
func (c *Client) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

// Identify reports the server flavour and version. `vizra doctor` prints this,
// because "the cache is up" is not a useful answer when the licence and the
// supported command set depend on which server answered.
func (c *Client) Identify(ctx context.Context) (ServerInfo, error) {
	raw, err := c.rdb.Info(ctx, "server").Result()
	if err != nil {
		return ServerInfo{Flavour: FlavourUnknown}, err
	}
	info := ServerInfo{Flavour: FlavourUnknown}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch k {
		case "valkey_version":
			info.Flavour = FlavourValkey
			info.Version = v
		case "redis_version":
			// Valkey also reports redis_version for compatibility, so it only
			// settles the flavour when no valkey_version was seen.
			if info.Flavour == FlavourUnknown {
				info.Flavour = FlavourRedis
				info.Version = v
			}
		case "server_name":
			if strings.EqualFold(v, "valkey") {
				info.Flavour = FlavourValkey
			}
		}
	}
	if info.Version == "" {
		return info, fmt.Errorf("cache: INFO server returned no version field")
	}
	return info, nil
}

// Key namespaces a cache key. Callers must not concatenate one by hand: the
// namespace comes from the site resolver (Q-008 checklist item 4).
func (c *Client) Key(parts ...string) string {
	return c.namespace + ":" + strings.Join(parts, ":")
}

// FlushAll empties the cache. Exported for the golden-path-after-FLUSHALL test
// and for `vizra doctor --repair`; it is never called on a request path.
func (c *Client) FlushAll(ctx context.Context) error { return c.rdb.FlushAll(ctx).Err() }
