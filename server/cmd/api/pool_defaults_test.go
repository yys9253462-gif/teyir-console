package main

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestApplyPoolDefaults(t *testing.T) {
	dsn := "postgres://u:p@db:5432/x?sslmode=disable"
	c, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	applyPoolDefaults(c, dsn)
	if c.MaxConns < defaultMaxConns {
		t.Fatalf("MaxConns = %d, want >= %d", c.MaxConns, defaultMaxConns)
	}

	dsn = dsn + "&pool_max_conns=3"
	c, err = pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	applyPoolDefaults(c, dsn)
	if c.MaxConns != 3 {
		t.Fatalf("explicit pool_max_conns overridden: %d", c.MaxConns)
	}
}
