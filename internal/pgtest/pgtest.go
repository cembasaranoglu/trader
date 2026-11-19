// Package pgtest gives tests a fresh PostgreSQL database with separate
// owner and application roles, on the server named by TRADER_TEST_PG (a
// superuser connection URL). Without it tests are skipped, unless
// TRADER_REQUIRE_PG=1 (CI), where they fail.
package pgtest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// DB is one test database. Super, Owner and App are connection strings.
type DB struct {
	Super, Owner, App string
	AppRole           string
	Name              string
}

// New creates the database and roles and drops them when the test ends.
func New(t testing.TB) *DB {
	t.Helper()
	base := os.Getenv("TRADER_TEST_PG")
	if base == "" {
		if os.Getenv("TRADER_REQUIRE_PG") == "1" {
			t.Fatal("TRADER_TEST_PG is not set but TRADER_REQUIRE_PG=1")
		}
		t.Skip("TRADER_TEST_PG not set")
	}
	ctx := context.Background()
	sc, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to TRADER_TEST_PG: %v", err)
	}
	defer sc.Close(ctx)
	var rb [6]byte
	if _, err := rand.Read(rb[:]); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(rb[:])
	d := &DB{Name: "t_" + id, AppRole: "a_" + id}
	owner := "o_" + id
	for _, st := range []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'pw'`, owner),
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'pw'`, d.AppRole),
		fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, d.Name, owner),
	} {
		if _, err := sc.Exec(ctx, st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		// Best effort: a leftover database only costs disk on a test server.
		_, _ = c.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, d.Name))
		_, _ = c.Exec(context.Background(), fmt.Sprintf(`DROP ROLE IF EXISTS %s`, owner))
		_, _ = c.Exec(context.Background(), fmt.Sprintf(`DROP ROLE IF EXISTS %s`, d.AppRole))
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(user *url.Userinfo) string {
		v := *u
		if user != nil {
			v.User = user
		}
		v.Path = "/" + d.Name
		return v.String()
	}
	d.Owner = mk(url.UserPassword(owner, "pw"))
	d.App = mk(url.UserPassword(d.AppRole, "pw"))
	d.Super = mk(nil)
	return d
}

// Exec runs SQL as the superuser on the test database.
func (d *DB) Exec(t testing.TB, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, d.Super)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// CAFile writes a throwaway self-signed CA certificate (PEM) for
// connection strings that need sslrootcert, and returns its path.
func CAFile(t testing.TB) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
