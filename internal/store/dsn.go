package store

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInsecureDSN reports a connection string to a non-local host without
// certificate verification.
var ErrInsecureDSN = errors.New("store: a non-local database requires sslmode=verify-full and sslrootcert")

// ParseDSN parses a PostgreSQL connection string (URL or key=value) and
// applies the store's session settings. A host that is not local (see
// IsLocalHost) must use TLS with certificate and host name verification and
// no plaintext fallback. Errors never include the connection string, which
// may hold a password.
func ParseDSN(dsn string, stmtTimeout, connectTimeout time.Duration) (*pgxpool.Config, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("store: empty database connection string")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgx errors can echo parts of the string; report only the kind.
		return nil, errors.New("store: invalid database connection string")
	}
	cc := cfg.ConnConfig
	if cc.Host == "" || cc.Database == "" || cc.User == "" {
		return nil, errors.New("store: database connection string needs host, dbname and user")
	}
	if !AllHostsLocal(cfg) {
		// Every attempt (the first host and each fallback, including
		// pgx's plaintext fallback for sslmode=prefer) must verify the
		// server certificate and host name against an explicit CA, not the
		// system trust store.
		verified := func(t *tls.Config) bool {
			return t != nil && !t.InsecureSkipVerify && t.ServerName != "" && t.RootCAs != nil
		}
		if !verified(cc.TLSConfig) {
			return nil, ErrInsecureDSN
		}
		for _, fb := range cc.Fallbacks {
			if !verified(fb.TLSConfig) {
				return nil, ErrInsecureDSN
			}
		}
	}
	if stmtTimeout <= 0 {
		stmtTimeout = defaultStmtTO
	}
	if connectTimeout <= 0 {
		connectTimeout = defaultConnectTO
	}
	cc.ConnectTimeout = connectTimeout
	if cc.RuntimeParams == nil {
		cc.RuntimeParams = map[string]string{}
	}
	cc.RuntimeParams["application_name"] = "traderd"
	cc.RuntimeParams["statement_timeout"] = strconv.FormatInt(stmtTimeout.Milliseconds(), 10)
	cc.RuntimeParams["lock_timeout"] = strconv.FormatInt(stmtTimeout.Milliseconds(), 10)
	cc.RuntimeParams["idle_in_transaction_session_timeout"] = idleInTxTimeoutMs
	cc.RuntimeParams["timezone"] = "UTC"
	// pg_temp last: the store's own unqualified queries never resolve to a
	// temporary table.
	cc.RuntimeParams["search_path"] = "public, pg_temp"
	return cfg, nil
}

// AllHostsLocal reports whether every host the configuration may dial
// (the first host and each fallback) is local (see IsLocalHost).
func AllHostsLocal(cfg *pgxpool.Config) bool {
	cc := cfg.ConnConfig
	if !IsLocalHost(cc.Host) {
		return false
	}
	for _, fb := range cc.Fallbacks {
		if !IsLocalHost(fb.Host) {
			return false
		}
	}
	return true
}

// IsLocalHost reports whether a database host is on this machine or on a
// private container network: a Unix socket directory, a loopback address,
// "localhost", or a single-label name such as a Docker Compose service
// ("postgres"). Such hosts may connect without TLS.
func IsLocalHost(host string) bool {
	if strings.HasPrefix(host, "/") {
		return true
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return !strings.Contains(host, ".") && !strings.Contains(host, ":") && host != ""
}

// RedactDSN returns host, port, database and user of a connection string
// for log messages, never the password.
func RedactDSN(dsn string) string {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return "(invalid connection string)"
	}
	cc := cfg.ConnConfig
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s", cc.Host, cc.Port, cc.Database, cc.User)
}
